package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	appconfig "github.com/udaykishore-resu/payments-platform/internal/application/config"
	appmerchant "github.com/udaykishore-resu/payments-platform/internal/application/merchant"
	apponboarding "github.com/udaykishore-resu/payments-platform/internal/application/onboarding"
	apppayment "github.com/udaykishore-resu/payments-platform/internal/application/payment"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	appwebhook "github.com/udaykishore-resu/payments-platform/internal/application/webhook"
	domainconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	domaingateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/health"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi"
	"github.com/udaykishore-resu/payments-platform/internal/transport/httpapi/handlers"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

const validBody = `{"merchantId":"mrc_01JB8Z11111111111111111111","amount":{"amount":1050,"currency":"USD"},` +
	`"paymentMethod":"CARD","paymentMethodReference":{"type":"GATEWAY_TOKEN","gatewayCode":"stripe","token":"tok_visa_ok"}}`

// TestCreatePayment covers the success path and every declared error status the handler itself
// can produce or forward.
//
// The 202 row is the one that matters most: a gateway timeout leaves the payment PROCESSING and
// this endpoint must answer 202, not 5xx. A client told a payment failed re-issues it with a
// fresh idempotency key, which is how one customer gets charged twice.
func TestCreatePayment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		body       string
		svc        func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error)
		wantStatus int
		wantCode   apierror.Code
		check      func(*testing.T, *httptest.ResponseRecorder)
	}{
		{
			name: "authorized is 201 with ETag and Location",
			body: validBody,
			svc: func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) {
				return okResult(newPayment()), nil
			},
			wantStatus: http.StatusCreated,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Header().Get(httpapi.HeaderETag) == "" {
					t.Error("no ETag on a created payment")
				}
				want := "https://api.example.com/v1/payments/" + testPaymentID.String()
				if got := rec.Header().Get(httpapi.HeaderLocation); got == "" {
					t.Error("no Location on a created payment")
				} else if got[:len("https://api.example.com/v1/payments/")] != want[:len("https://api.example.com/v1/payments/")] {
					t.Errorf("Location = %q, want the configured base URL", got)
				}
				var p httpapi.Payment
				mustJSON(t, rec.Body.Bytes(), &p)
				if p.State != "AUTHORIZED" {
					t.Errorf("state = %q, want AUTHORIZED", p.State)
				}
				if p.Amount.Amount != 1050 || p.Amount.Currency != "USD" {
					t.Errorf("amount = %+v, want 1050 USD", p.Amount)
				}
				if p.RiskDecision != "ALLOW" {
					t.Errorf("riskDecision = %q, want ALLOW", p.RiskDecision)
				}
			},
		},
		{
			name: "gateway timeout is 202 with Retry-After, never an error",
			body: validBody,
			svc: func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) {
				return okResult(newProcessingPayment()), nil
			},
			wantStatus: http.StatusAccepted,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if rec.Header().Get(httpapi.HeaderRetryAfter) == "" {
					t.Error("202 carries no Retry-After; the client does not know to poll")
				}
				var p httpapi.Payment
				mustJSON(t, rec.Body.Bytes(), &p)
				if p.State != "PROCESSING" {
					t.Errorf("state = %q, want PROCESSING", p.State)
				}
				if !p.ReconciliationRequired {
					t.Error("a TIMEOUT_UNKNOWN attempt must set reconciliationRequired")
				}
			},
		},
		{
			name:       "malformed body is 400",
			body:       `{`,
			wantStatus: http.StatusBadRequest,
			wantCode:   apierror.CodeMalformedRequest,
		},
		{
			name:       "unknown field is 400 VALIDATION_FAILED naming the field",
			body:       `{"merchantId":"mrc_01JB8Z11111111111111111111","statementDescripter":"x"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   apierror.CodeValidationFailed,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				var p httpapi.Problem
				mustJSON(t, rec.Body.Bytes(), &p)
				if len(p.Details) == 0 || p.Details[0].Field != "statementDescripter" {
					t.Errorf("details do not name the misspelled field: %+v", p.Details)
				}
			},
		},
		{
			name: "zero amount is 422 AMOUNT_INVALID",
			body: `{"merchantId":"mrc_01JB8Z11111111111111111111","amount":{"amount":0,"currency":"USD"},` +
				`"paymentMethod":"CARD","paymentMethodReference":{"type":"GATEWAY_TOKEN","token":"tok_x"}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeAmountInvalid,
		},
		{
			name: "unsupported currency is 422 CURRENCY_NOT_SUPPORTED",
			body: `{"merchantId":"mrc_01JB8Z11111111111111111111","amount":{"amount":100,"currency":"XXY"},` +
				`"paymentMethod":"CARD","paymentMethodReference":{"type":"GATEWAY_TOKEN","token":"tok_x"}}`,
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeCurrencyNotSupported,
		},
		{
			name:       "merchant not found is 404",
			body:       validBody,
			svc:        errCreate(apierror.New(apierror.CodeMerchantNotFound, "no such merchant")),
			wantStatus: http.StatusNotFound,
			wantCode:   apierror.CodeMerchantNotFound,
		},
		{
			name:       "suspended merchant is 409",
			body:       validBody,
			svc:        errCreate(apierror.New(apierror.CodeMerchantNotActive, "merchant is suspended")),
			wantStatus: http.StatusConflict,
			wantCode:   apierror.CodeMerchantNotActive,
		},
		{
			name:       "risk decline is 422",
			body:       validBody,
			svc:        errCreate(apierror.New(apierror.CodeRiskDeclined, "declined by policy")),
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierror.CodeRiskDeclined,
		},
		{
			name:       "no eligible gateway is 503 and retryable",
			body:       validBody,
			svc:        errCreate(apierror.New(apierror.CodeNoEligibleGateway, "no gateway can serve this")),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierror.CodeNoEligibleGateway,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				var p httpapi.Problem
				mustJSON(t, rec.Body.Bytes(), &p)
				if !p.Retryable {
					t.Error("503 must be marked retryable; SDKs branch on this flag")
				}
			},
		},
		{
			name:       "gateway contract violation is 502",
			body:       validBody,
			svc:        errCreate(apierror.New(apierror.CodeGatewayContractViolation, "amount echo mismatch")),
			wantStatus: http.StatusBadGateway,
			wantCode:   apierror.CodeGatewayContractViolation,
		},
		{
			name:       "internal error is 500 and never leaks the cause",
			body:       validBody,
			svc:        errCreate(apierror.Wrap(errSentinel, apierror.CodeInternalError, "the request could not be completed")),
			wantStatus: http.StatusInternalServerError,
			wantCode:   apierror.CodeInternalError,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				if strings.Contains(rec.Body.String(), "sentinel-internal-detail") {
					t.Errorf("the wrapped cause leaked into the response: %s", rec.Body)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := tc.svc
			if svc == nil {
				svc = func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) {
					return okResult(newPayment()), nil
				}
			}
			rt := newRouter(handlers.Deps{Payments: &fakePayments{create: svc}})
			rec := do(rt, http.MethodPost, "/v1/payments", tc.body,
				map[string]string{httpapi.HeaderIdempotencyKey: "k1"})

			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
			if tc.check != nil {
				tc.check(t, rec)
			}
		})
	}
}

// TestGetPayment covers the read, the conditional read and the not-found path.
func TestGetPayment(t *testing.T) {
	t.Parallel()
	p := newPayment()
	etag := strconv.Quote(strconv.FormatInt(int64(p.Version()), 10))

	tests := []struct {
		name       string
		path       string
		headers    map[string]string
		svc        func(context.Context, shared.PaymentID) (*payment.Payment, error)
		wantStatus int
		wantCode   apierror.Code
	}{
		{
			name: "found is 200 with ETag", path: "/v1/payments/" + testPaymentID.String(),
			wantStatus: http.StatusOK,
		},
		{
			name:       "matching If-None-Match is 304 with no body",
			path:       "/v1/payments/" + testPaymentID.String(),
			headers:    map[string]string{httpapi.HeaderIfNoneMatch: etag},
			wantStatus: http.StatusNotModified,
		},
		{
			name:       "stale If-None-Match is 200",
			path:       "/v1/payments/" + testPaymentID.String(),
			headers:    map[string]string{httpapi.HeaderIfNoneMatch: `"0"`},
			wantStatus: http.StatusOK,
		},
		{
			name: "malformed id is 400", path: "/v1/payments/not-a-ulid",
			wantStatus: http.StatusBadRequest,
		},
		{
			name: "not found is 404", path: "/v1/payments/" + testPaymentID.String(),
			svc: func(context.Context, shared.PaymentID) (*payment.Payment, error) {
				return nil, apierror.New(apierror.CodePaymentNotFound, "no such payment")
			},
			wantStatus: http.StatusNotFound, wantCode: apierror.CodePaymentNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := tc.svc
			if svc == nil {
				svc = func(context.Context, shared.PaymentID) (*payment.Payment, error) { return p, nil }
			}
			rt := newRouter(handlers.Deps{Payments: &fakePayments{get: svc}})
			rec := do(rt, http.MethodGet, tc.path, "", tc.headers)

			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
			if tc.wantStatus == http.StatusNotModified {
				if rec.Body.Len() != 0 {
					t.Errorf("304 carried a body: %s", rec.Body)
				}
				if rec.Header().Get(httpapi.HeaderETag) == "" {
					t.Error("304 carries no ETag; the client has no token for its next request")
				}
			}
		})
	}
}

// TestListPayments covers pagination and the filter parsing that must reject rather than ignore.
func TestListPayments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		query      string
		wantStatus int
		wantNext   bool
	}{
		{"defaults", "", http.StatusOK, true},
		{"explicit limit", "?limit=10", http.StatusOK, true},
		{"limit above the maximum is rejected, not clamped", "?limit=10000", http.StatusBadRequest, false},
		{"limit of zero is rejected", "?limit=0", http.StatusBadRequest, false},
		{"non-numeric limit is rejected", "?limit=many", http.StatusBadRequest, false},
		{"unknown state is rejected, never silently ignored", "?state=NOT_A_STATE", http.StatusBadRequest, false},
		{"known state is accepted", "?state=AUTHORIZED&state=CAPTURED", http.StatusOK, true},
		{"bad timestamp is rejected", "?createdAfter=yesterday", http.StatusBadRequest, false},
		{"rfc3339 timestamp is accepted", "?createdAfter=2026-08-01T00:00:00Z", http.StatusOK, true},
		{"bad merchant id is rejected", "?merchantId=nope", http.StatusBadRequest, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(handlers.Deps{Payments: &fakePayments{
				list: func(context.Context, ports.PaymentFilter, ports.Page) ([]*payment.Payment, string, error) {
					return []*payment.Payment{newPayment()}, "cursor_next", nil
				},
			}})
			rec := do(rt, http.MethodGet, "/v1/payments"+tc.query, "", nil)
			assertStatus(t, rec, tc.wantStatus, "")
			if tc.wantStatus != http.StatusOK {
				return
			}
			var page struct {
				Data       []httpapi.Payment `json:"data"`
				NextCursor *string           `json:"next_cursor"`
			}
			mustJSON(t, rec.Body.Bytes(), &page)
			if len(page.Data) != 1 {
				t.Fatalf("data has %d items, want 1", len(page.Data))
			}
			if tc.wantNext && (page.NextCursor == nil || *page.NextCursor != "cursor_next") {
				t.Errorf("next_cursor = %v, want cursor_next", page.NextCursor)
			}
			// A listing must not embed attempts: a page of 200 payments with a dozen attempts
			// each is a megabyte-scale response for a table of amounts and states.
			if len(page.Data[0].Attempts) != 0 {
				t.Error("a listing embedded attempts")
			}
		})
	}
}

// TestListPaymentsNullCursorOnLastPage pins the contract's "next_cursor is null, never absent"
// rule: a client branching on the value must not have to distinguish a missing key from a null.
func TestListPaymentsNullCursorOnLastPage(t *testing.T) {
	t.Parallel()
	rt := newRouter(handlers.Deps{Payments: &fakePayments{
		list: func(context.Context, ports.PaymentFilter, ports.Page) ([]*payment.Payment, string, error) {
			return nil, "", nil
		},
	}})
	rec := do(rt, http.MethodGet, "/v1/payments", "", nil)
	var raw map[string]json.RawMessage
	mustJSON(t, rec.Body.Bytes(), &raw)
	if _, ok := raw["next_cursor"]; !ok {
		t.Fatal("next_cursor is absent on the last page; the contract requires it to be present and null")
	}
	if string(raw["next_cursor"]) != "null" {
		t.Errorf("next_cursor = %s, want null", raw["next_cursor"])
	}
	if string(raw["data"]) != "[]" {
		t.Errorf("data = %s, want an empty array rather than null", raw["data"])
	}
}

// TestCaptureRefundVoid covers the three mutating money operations together, because their status
// mapping and their error surface are the same shape.
func TestCaptureRefundVoid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		deps       handlers.Deps
		wantStatus int
		wantCode   apierror.Code
	}{
		{
			name: "capture full amount is 200", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/capture", body: `{}`,
			deps:       handlers.Deps{Payments: &fakePayments{capture: okCapture()}},
			wantStatus: http.StatusOK,
		},
		{
			name: "capture with explicit amount is 200", method: http.MethodPost,
			path:       "/v1/payments/" + testPaymentID.String() + "/capture",
			body:       `{"amount":{"amount":500,"currency":"USD"},"isFinalCapture":false}`,
			deps:       handlers.Deps{Payments: &fakePayments{capture: okCapture()}},
			wantStatus: http.StatusOK,
		},
		{
			name: "capture with zero amount is 422", method: http.MethodPost,
			path:       "/v1/payments/" + testPaymentID.String() + "/capture",
			body:       `{"amount":{"amount":0,"currency":"USD"}}`,
			deps:       handlers.Deps{Payments: &fakePayments{capture: okCapture()}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: apierror.CodeAmountInvalid,
		},
		{
			name: "capture exceeding authorization is 422", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/capture", body: `{}`,
			deps: handlers.Deps{Payments: &fakePayments{
				capture: func(context.Context, apppayment.CaptureCommand) (*apppayment.Result, error) {
					return nil, apierror.New(apierror.CodeCaptureExceedsAuthorized, "over the hold")
				},
			}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: apierror.CodeCaptureExceedsAuthorized,
		},
		{
			name: "capture of a captured payment is 409", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/capture", body: `{}`,
			deps: handlers.Deps{Payments: &fakePayments{
				capture: func(context.Context, apppayment.CaptureCommand) (*apppayment.Result, error) {
					return nil, apierror.New(apierror.CodePaymentAlreadyProcessed, "already captured")
				},
			}},
			wantStatus: http.StatusConflict, wantCode: apierror.CodePaymentAlreadyProcessed,
		},
		{
			name: "refund is 201 with Location", method: http.MethodPost,
			path:       "/v1/payments/" + testPaymentID.String() + "/refund",
			body:       `{"amount":{"amount":500,"currency":"USD"},"reason":"REQUESTED_BY_CUSTOMER"}`,
			deps:       handlers.Deps{Payments: &fakePayments{refund: okRefund(true)}},
			wantStatus: http.StatusCreated,
		},
		{
			name: "unconfirmed refund is 202", method: http.MethodPost,
			path:       "/v1/payments/" + testPaymentID.String() + "/refund",
			body:       `{"reason":"DUPLICATE"}`,
			deps:       handlers.Deps{Payments: &fakePayments{refund: okRefund(false)}},
			wantStatus: http.StatusAccepted,
		},
		{
			name: "refund without a reason is 400", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/refund", body: `{}`,
			deps:       handlers.Deps{Payments: &fakePayments{refund: okRefund(true)}},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "refund exceeding captures is 422", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/refund", body: `{"reason":"OTHER"}`,
			deps: handlers.Deps{Payments: &fakePayments{
				refund: func(context.Context, apppayment.RefundCommand) (*apppayment.Result, error) {
					return nil, apierror.New(apierror.CodeRefundExceedsCaptured, "invariant I1")
				},
			}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: apierror.CodeRefundExceedsCaptured,
		},
		{
			name: "void is 200", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/void",
			body: `{"reason":"MERCHANT_REQUEST"}`,
			deps: handlers.Deps{Payments: &fakePayments{
				void: func(context.Context, apppayment.VoidCommand) (*apppayment.Result, error) {
					p := newPayment()
					if err := p.Void(testClock); err != nil {
						t.Fatalf("Void: %v", err)
					}
					return okResult(p), nil
				},
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "void without a reason is 400", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/void", body: `{}`,
			deps: handlers.Deps{Payments: &fakePayments{
				void: func(context.Context, apppayment.VoidCommand) (*apppayment.Result, error) {
					return okResult(newPayment()), nil
				},
			}},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "void of a captured payment is 409", method: http.MethodPost,
			path: "/v1/payments/" + testPaymentID.String() + "/void",
			body: `{"reason":"MERCHANT_REQUEST"}`,
			deps: handlers.Deps{Payments: &fakePayments{
				void: func(context.Context, apppayment.VoidCommand) (*apppayment.Result, error) {
					return nil, apierror.New(apierror.CodeInvalidStateTransition, "cannot void a capture")
				},
			}},
			wantStatus: http.StatusConflict, wantCode: apierror.CodeInvalidStateTransition,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(tc.deps)
			rec := do(rt, tc.method, tc.path, tc.body,
				map[string]string{httpapi.HeaderIdempotencyKey: "k1"})
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestMerchantHandlers covers the control-plane merchant surface.
func TestMerchantHandlers(t *testing.T) {
	t.Parallel()
	m := newMerchant()
	etag := appmerchant.ETag(m)
	const createBody = `{"displayName":"Acme","residencyRegion":"eu","businessProfile":{` +
		`"legalName":"Acme Trading Ltd","entityType":"PRIVATE_LIMITED","registrationNumber":"12345678",` +
		`"incorporationCountry":"DE","mcc":"5411","declaredMonthlyVolume":{"amount":1000000,"currency":"EUR"}}}`

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		headers    map[string]string
		deps       handlers.Deps
		wantStatus int
		wantCode   apierror.Code
	}{
		{
			name: "create is 201", method: http.MethodPost, path: "/v1/merchants", body: createBody,
			deps: handlers.Deps{Merchants: &fakeMerchants{
				create: func(context.Context, appmerchant.CreateCommand) (*merchant.Merchant, error) { return m, nil },
			}},
			wantStatus: http.StatusCreated,
		},
		{
			name:   "a tenantId in the body is rejected as an unknown field",
			method: http.MethodPost, path: "/v1/merchants",
			body: `{"tenantId":"ten_other","displayName":"Acme","residencyRegion":"eu","businessProfile":{}}`,
			deps: handlers.Deps{Merchants: &fakeMerchants{
				create: func(context.Context, appmerchant.CreateCommand) (*merchant.Merchant, error) { return m, nil },
			}},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "duplicate external reference is 409", method: http.MethodPost,
			path: "/v1/merchants", body: createBody,
			deps: handlers.Deps{Merchants: &fakeMerchants{
				create: func(context.Context, appmerchant.CreateCommand) (*merchant.Merchant, error) {
					return nil, apierror.New(apierror.CodeMerchantAlreadyExists, "already registered")
				},
			}},
			wantStatus: http.StatusConflict, wantCode: apierror.CodeMerchantAlreadyExists,
		},
		{
			name: "get is 200", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String(),
			deps: handlers.Deps{Merchants: &fakeMerchants{
				get: func(context.Context, shared.TenantID, shared.MerchantID) (*merchant.Merchant, error) { return m, nil },
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "conditional get is 304", method: http.MethodGet,
			path:    "/v1/merchants/" + testMerchantID.String(),
			headers: map[string]string{httpapi.HeaderIfNoneMatch: etag},
			deps: handlers.Deps{Merchants: &fakeMerchants{
				get: func(context.Context, shared.TenantID, shared.MerchantID) (*merchant.Merchant, error) { return m, nil },
			}},
			wantStatus: http.StatusNotModified,
		},
		{
			name: "get of an unknown merchant is 404", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String(),
			deps: handlers.Deps{Merchants: &fakeMerchants{
				get: func(context.Context, shared.TenantID, shared.MerchantID) (*merchant.Merchant, error) {
					return nil, apierror.New(apierror.CodeMerchantNotFound, "no such merchant")
				},
			}},
			wantStatus: http.StatusNotFound, wantCode: apierror.CodeMerchantNotFound,
		},
		{
			name: "list is 200", method: http.MethodGet, path: "/v1/merchants",
			deps: handlers.Deps{Merchants: &fakeMerchants{
				list: func(context.Context, shared.TenantID, ports.MerchantFilter, ports.Page) ([]*merchant.Merchant, string, error) {
					return []*merchant.Merchant{m}, "", nil
				},
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "list with an unknown status is 400", method: http.MethodGet,
			path: "/v1/merchants?status=NOPE",
			deps: handlers.Deps{Merchants: &fakeMerchants{
				list: func(context.Context, shared.TenantID, ports.MerchantFilter, ports.Page) ([]*merchant.Merchant, string, error) {
					return nil, "", nil
				},
			}},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "patch without If-Match is 428, not 412", method: http.MethodPatch,
			path: "/v1/merchants/" + testMerchantID.String(), body: `{"displayName":"New"}`,
			deps: handlers.Deps{Merchants: &fakeMerchants{
				update: func(context.Context, appmerchant.UpdateCommand) (*merchant.Merchant, error) { return m, nil },
			}},
			wantStatus: http.StatusPreconditionRequired, wantCode: apierror.CodePreconditionRequired,
		},
		{
			name: "patch with a stale If-Match is 412", method: http.MethodPatch,
			path: "/v1/merchants/" + testMerchantID.String(), body: `{}`,
			headers: map[string]string{httpapi.HeaderIfMatch: `"0"`},
			deps: handlers.Deps{Merchants: &fakeMerchants{
				update: func(context.Context, appmerchant.UpdateCommand) (*merchant.Merchant, error) {
					return nil, apierror.New(apierror.CodePreconditionRequired, "stale etag").
						WithMessage("the resource changed since you read it")
				},
			}},
			wantStatus: http.StatusPreconditionRequired,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(tc.deps)
			headers := map[string]string{httpapi.HeaderIdempotencyKey: "k1"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			rec := do(rt, tc.method, tc.path, tc.body, headers)
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestOnboardingHandlers covers the workflow surface, including the deliberate 200-not-409 on a
// repeated start.
func TestOnboardingHandlers(t *testing.T) {
	t.Parallel()
	c := newCase()
	const startBody = `{"selectedGateways":["stripe"],"environment":"sandbox"}`

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		deps       handlers.Deps
		wantStatus int
		wantCode   apierror.Code
	}{
		{
			name: "first start is 201", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding", body: startBody,
			deps: handlers.Deps{
				Onboarding: &fakeOnboarding{
					start: func(context.Context, apponboarding.StartCommand) (*apponboarding.Case, error) { return c, nil },
					get: func(context.Context, shared.TenantID, shared.WorkflowID) (*apponboarding.Case, error) {
						return nil, errSentinel
					},
				},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusCreated,
		},
		{
			name: "second start is 200 with the existing case, never 409", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding", body: startBody,
			deps: handlers.Deps{
				Onboarding: &fakeOnboarding{
					start: func(context.Context, apponboarding.StartCommand) (*apponboarding.Case, error) { return c, nil },
					get:   func(context.Context, shared.TenantID, shared.WorkflowID) (*apponboarding.Case, error) { return c, nil },
				},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "start with no gateways is 400", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding",
			body: `{"selectedGateways":[],"environment":"sandbox"}`,
			deps: handlers.Deps{
				Onboarding:       &fakeOnboarding{},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "get is 200", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding",
			deps: handlers.Deps{
				Onboarding: &fakeOnboarding{
					get: func(context.Context, shared.TenantID, shared.WorkflowID) (*apponboarding.Case, error) { return c, nil },
				},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "get with no case is 404", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding",
			deps: handlers.Deps{
				Onboarding: &fakeOnboarding{},
				OnboardingLookup: &fakeLookup{
					workflow: func(context.Context, shared.TenantID, shared.MerchantID) (shared.WorkflowID, error) {
						return "", apierror.New(apierror.CodeOnboardingCaseNotFound, "no case")
					},
				},
			},
			wantStatus: http.StatusNotFound, wantCode: apierror.CodeOnboardingCaseNotFound,
		},
		{
			name: "signal is 202", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding/signals/compliance-approval",
			body: `{"decision":"APPROVE","reviewerId":"usr_1","reason":"documents verified"}`,
			deps: handlers.Deps{
				Onboarding: &fakeOnboarding{
					signal: func(context.Context, apponboarding.SignalCommand) (*apponboarding.Case, error) { return c, nil },
				},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name: "unknown signal name is 409 WORKFLOW_SIGNAL_NOT_EXPECTED", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding/signals/not-a-signal",
			body: `{"decision":"APPROVE"}`,
			deps: handlers.Deps{
				Onboarding:       &fakeOnboarding{},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusConflict, wantCode: apierror.CodeWorkflowSignalNotExpected,
		},
		{
			name: "signal without a decision is 400", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/onboarding/signals/kyc-decision",
			body: `{}`,
			deps: handlers.Deps{
				Onboarding:       &fakeOnboarding{},
				OnboardingLookup: &fakeLookup{},
			},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(tc.deps)
			rec := do(rt, tc.method, tc.path, tc.body,
				map[string]string{httpapi.HeaderIdempotencyKey: "k1"})
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestConfigurationHandlers covers the desired-state surface.
func TestConfigurationHandlers(t *testing.T) {
	t.Parallel()
	c := newConfig()
	putBody, err := json.Marshal(httpapi.MerchantConfigurationOf(c))
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		headers    map[string]string
		deps       handlers.Deps
		wantStatus int
		wantCode   apierror.Code
	}{
		{
			name: "get active is 200", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration",
			deps: handlers.Deps{Configuration: &fakeConfig{
				getActive: func(context.Context, shared.TenantID, shared.MerchantID) (*domainconfig.MerchantConfig, error) {
					return c, nil
				},
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "conditional get is 304", method: http.MethodGet,
			path:    "/v1/merchants/" + testMerchantID.String() + "/configuration",
			headers: map[string]string{httpapi.HeaderIfNoneMatch: c.ETag()},
			deps: handlers.Deps{Configuration: &fakeConfig{
				getActive: func(context.Context, shared.TenantID, shared.MerchantID) (*domainconfig.MerchantConfig, error) {
					return c, nil
				},
			}},
			wantStatus: http.StatusNotModified,
		},
		{
			name: "put without If-Match is 428", method: http.MethodPut,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration",
			body: string(putBody),
			deps: handlers.Deps{Configuration: &fakeConfig{
				publish: func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) { return c, nil },
			}},
			wantStatus: http.StatusPreconditionRequired,
		},
		{
			name: "put with If-Match is 200", method: http.MethodPut,
			path:    "/v1/merchants/" + testMerchantID.String() + "/configuration",
			body:    string(putBody),
			headers: map[string]string{httpapi.HeaderIfMatch: c.ETag()},
			deps: handlers.Deps{Configuration: &fakeConfig{
				publish: func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) { return c, nil },
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "concurrent publish loses with 412", method: http.MethodPut,
			path:    "/v1/merchants/" + testMerchantID.String() + "/configuration",
			body:    string(putBody),
			headers: map[string]string{httpapi.HeaderIfMatch: `"stale"`},
			deps: handlers.Deps{Configuration: &fakeConfig{
				publish: func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) {
					return nil, apierror.New(apierror.CodeConfigurationVersionConflict, "someone else published")
				},
			}},
			wantStatus: http.StatusPreconditionFailed, wantCode: apierror.CodeConfigurationVersionConflict,
		},
		{
			name: "invalid document is 422", method: http.MethodPut,
			path:    "/v1/merchants/" + testMerchantID.String() + "/configuration",
			body:    string(putBody),
			headers: map[string]string{httpapi.HeaderIfMatch: c.ETag()},
			deps: handlers.Deps{Configuration: &fakeConfig{
				publish: func(context.Context, appconfig.PublishCommand) (*domainconfig.MerchantConfig, error) {
					return nil, apierror.New(apierror.CodeConfigurationInvalid, "primary gateway is not connected")
				},
			}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: apierror.CodeConfigurationInvalid,
		},
		{
			name: "list versions is 200", method: http.MethodGet,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration/versions",
			deps: handlers.Deps{Configuration: &fakeConfig{
				listVersions: func(context.Context, shared.TenantID, shared.MerchantID, ports.Page) ([]*domainconfig.MerchantConfig, string, error) {
					return []*domainconfig.MerchantConfig{c}, "", nil
				},
			}},
			wantStatus: http.StatusOK,
		},
		{
			name: "rollback is 201 — an append, never a deletion", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration/rollback",
			body: `{"toVersion":2,"reason":"routing regression in v3"}`,
			deps: handlers.Deps{Configuration: &fakeConfig{
				rollback: func(context.Context, appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error) { return c, nil },
			}},
			wantStatus: http.StatusCreated,
		},
		{
			name: "rollback without a reason is 400", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration/rollback",
			body: `{"toVersion":2,"reason":"  "}`,
			deps: handlers.Deps{Configuration: &fakeConfig{
				rollback: func(context.Context, appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error) { return c, nil },
			}},
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "rollback to a version that no longer validates is 422", method: http.MethodPost,
			path: "/v1/merchants/" + testMerchantID.String() + "/configuration/rollback",
			body: `{"toVersion":2,"reason":"regression"}`,
			deps: handlers.Deps{Configuration: &fakeConfig{
				rollback: func(context.Context, appconfig.RollbackCommand) (*domainconfig.MerchantConfig, error) {
					return nil, apierror.New(apierror.CodeConfigurationInvalid, "the target names a de-registered gateway")
				},
			}},
			wantStatus: http.StatusUnprocessableEntity, wantCode: apierror.CodeConfigurationInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(tc.deps)
			headers := map[string]string{httpapi.HeaderIdempotencyKey: "k1"}
			for k, v := range tc.headers {
				headers[k] = v
			}
			rec := do(rt, tc.method, tc.path, tc.body, headers)
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
		})
	}
}

// TestGatewayHandlers covers the catalogue, health and rotation.
func TestGatewayHandlers(t *testing.T) {
	t.Parallel()
	g := newGateway()
	deps := handlers.Deps{Gateways: &fakeGateways{
		get:  func(context.Context, shared.GatewayID) (*domaingateway.Gateway, error) { return g, nil },
		list: func(context.Context) ([]*domaingateway.Gateway, error) { return []*domaingateway.Gateway{g}, nil },
		health: func(context.Context, shared.GatewayID, []shared.Operation) ([]*domaingateway.Health, error) {
			return []*domaingateway.Health{newGatewayHealth()}, nil
		},
		rotate: func(context.Context, handlers.RotateCommand) (*handlers.RotationAccepted, error) {
			return &handlers.RotationAccepted{
				ConnectionID: "gwc_01JB8Z44444444444444444444",
				State:        "DUAL_RUN",
				StartedAt:    testClock.Now(),
			}, nil
		},
	}}

	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
		wantCode   apierror.Code
		check      func(*testing.T, *httptest.ResponseRecorder)
	}{
		{name: "list is 200", method: http.MethodGet, path: "/v1/gateways", wantStatus: http.StatusOK},
		{name: "list filtered by currency", method: http.MethodGet, path: "/v1/gateways?currency=EUR", wantStatus: http.StatusOK},
		{
			name: "list filtered to nothing is an empty page", method: http.MethodGet,
			path: "/v1/gateways?currency=JPY", wantStatus: http.StatusOK,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				var page struct {
					Data []httpapi.Gateway `json:"data"`
				}
				mustJSON(t, rec.Body.Bytes(), &page)
				if len(page.Data) != 0 {
					t.Errorf("data has %d items, want 0", len(page.Data))
				}
			},
		},
		{name: "get is 200", method: http.MethodGet, path: "/v1/gateways/stripe", wantStatus: http.StatusOK},
		{
			name: "health is 200 with an operations array", method: http.MethodGet,
			path: "/v1/gateways/stripe/health", wantStatus: http.StatusOK,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				var report httpapi.GatewayHealthReport
				mustJSON(t, rec.Body.Bytes(), &report)
				if len(report.Operations) != 1 {
					t.Fatalf("operations has %d entries, want 1", len(report.Operations))
				}
				if report.Operations[0].Operation != "AUTHORIZE" {
					t.Errorf("operation = %q, want AUTHORIZE", report.Operations[0].Operation)
				}
			},
		},
		{
			name: "health with an unknown operation filter is 400", method: http.MethodGet,
			path:       "/v1/gateways/stripe/health?operation=SETTLE",
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
		{
			name: "rotate is 202 and never returns credential material", method: http.MethodPost,
			path:       "/v1/gateways/stripe/credentials:rotate",
			body:       `{"merchantId":"mrc_01JB8Z11111111111111111111","environment":"SANDBOX","reason":"SCHEDULED"}`,
			wantStatus: http.StatusAccepted,
			check: func(t *testing.T, rec *httptest.ResponseRecorder) {
				var acc httpapi.CredentialRotationAccepted
				mustJSON(t, rec.Body.Bytes(), &acc)
				if acc.State != "DUAL_RUN" {
					t.Errorf("state = %q, want DUAL_RUN", acc.State)
				}
				if strings.Contains(rec.Body.String(), "secret") && !strings.Contains(rec.Body.String(), "secret://") {
					t.Errorf("rotation response may carry a reference but never material: %s", rec.Body)
				}
			},
		},
		{
			name: "rotate with an unknown reason is 400", method: http.MethodPost,
			path:       "/v1/gateways/stripe/credentials:rotate",
			body:       `{"merchantId":"mrc_01JB8Z11111111111111111111","environment":"SANDBOX","reason":"BECAUSE"}`,
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(deps)
			rec := do(rt, tc.method, tc.path, tc.body,
				map[string]string{httpapi.HeaderIdempotencyKey: "k1"})
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
			if tc.check != nil {
				tc.check(t, rec)
			}
		})
	}
}

// TestWebhookIngress covers the gateway ingress, including the Adyen protocol body.
func TestWebhookIngress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		path       string
		body       string
		svc        func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error)
		wantStatus int
		wantBody   string
		wantCode   apierror.Code
	}{
		{
			name: "accepted is 202", path: "/v1/webhooks/stripe", body: `{"id":"evt_1","type":"charge.succeeded"}`,
			svc: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return &appwebhook.Accepted{WebhookID: "whk_01JB8Z55555555555555555555"}, nil
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name: "duplicate is 200, not an error — otherwise the gateway retries forever",
			path: "/v1/webhooks/stripe", body: `{"id":"evt_1"}`,
			svc: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return &appwebhook.Accepted{WebhookID: "whk_1", Duplicate: true}, nil
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "adyen gets the [accepted] body its protocol requires",
			path: "/v1/webhooks/adyen", body: `{"notificationItems":[]}`,
			svc: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return &appwebhook.Accepted{WebhookID: "whk_2"}, nil
			},
			wantStatus: http.StatusAccepted, wantBody: "[accepted]",
		},
		{
			name: "bad signature is 401", path: "/v1/webhooks/stripe", body: `{"id":"evt_1"}`,
			svc: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return nil, apierror.New(apierror.CodeWebhookSignatureInvalid, "signature does not verify")
			},
			wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeWebhookSignatureInvalid,
		},
		{
			name: "replay outside the window is 401", path: "/v1/webhooks/stripe", body: `{"id":"evt_1"}`,
			svc: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
				return nil, apierror.New(apierror.CodeWebhookReplayDetected, "timestamp outside the window")
			},
			wantStatus: http.StatusUnauthorized, wantCode: apierror.CodeWebhookReplayDetected,
		},
		{
			name: "empty body is 400", path: "/v1/webhooks/stripe", body: "",
			wantStatus: http.StatusBadRequest, wantCode: apierror.CodeValidationFailed,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			svc := tc.svc
			if svc == nil {
				svc = func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
					return &appwebhook.Accepted{}, nil
				}
			}
			rt := newRouter(handlers.Deps{Webhooks: &fakeWebhooks{ingest: svc}})
			rec := do(rt, http.MethodPost, tc.path, tc.body, nil)
			assertStatus(t, rec, tc.wantStatus, tc.wantCode)
			if tc.wantBody != "" && rec.Body.String() != tc.wantBody {
				t.Errorf("body = %q, want %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestWebhookPassesExactBytes asserts the verifier receives the received octets rather than a
// re-encoding. Any re-serialisation — key reordering, whitespace, a number rendered differently —
// invalidates the gateway's HMAC.
func TestWebhookPassesExactBytes(t *testing.T) {
	t.Parallel()
	const body = `{"z":1,  "a":  2}`
	var seen []byte
	rt := newRouter(handlers.Deps{Webhooks: &fakeWebhooks{
		ingest: func(_ context.Context, r appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
			seen = r.Raw
			return &appwebhook.Accepted{WebhookID: "whk_1"}, nil
		},
	}})
	do(rt, http.MethodPost, "/v1/webhooks/stripe", body, nil)
	if string(seen) != body {
		t.Errorf("verifier received %q, want the exact received bytes %q", seen, body)
	}
}

// TestProbes covers the three distinct semantics of /livez, /readyz and /healthz.
func TestProbes(t *testing.T) {
	t.Parallel()
	down := healthDown()
	tests := []struct {
		name       string
		path       string
		deps       handlers.Deps
		wantStatus int
		wantLabel  string
	}{
		{
			name: "livez is 200 when the process is healthy", path: "/livez",
			wantStatus: http.StatusOK, wantLabel: "ok",
		},
		{
			name:       "livez stays 200 when a dependency is down — liveness never checks downstreams",
			path:       "/livez",
			deps:       handlers.Deps{Health: &fakeHealth{live: healthUp(), ready: down}},
			wantStatus: http.StatusOK, wantLabel: "ok",
		},
		{
			name: "livez stays 200 while draining — a draining process is not wedged",
			path: "/livez",
			deps: handlers.Deps{
				Health:   &fakeHealth{live: healthUp(), ready: healthUp()},
				Draining: func() bool { return true },
			},
			wantStatus: http.StatusOK,
		},
		{
			name: "readyz is 503 when a dependency is down", path: "/readyz",
			deps:       handlers.Deps{Health: &fakeHealth{live: healthUp(), ready: down}},
			wantStatus: http.StatusServiceUnavailable, wantLabel: "unavailable",
		},
		{
			name: "readyz is 503 while draining, before anything is closed", path: "/readyz",
			deps: handlers.Deps{
				Health:   &fakeHealth{live: healthUp(), ready: healthUp()},
				Draining: func() bool { return true },
			},
			wantStatus: http.StatusServiceUnavailable, wantLabel: "unavailable",
		},
		{
			name: "healthz is 200 while draining, with the drain visible in checks", path: "/healthz",
			deps: handlers.Deps{
				Health:   &fakeHealth{live: healthUp(), ready: healthUp()},
				Draining: func() bool { return true },
			},
			wantStatus: http.StatusOK, wantLabel: "degraded",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt := newRouter(tc.deps)
			rec := do(rt, http.MethodGet, tc.path, "", nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.wantStatus, rec.Body)
			}
			var s httpapi.HealthStatus
			mustJSON(t, rec.Body.Bytes(), &s)
			if tc.wantLabel != "" && s.Status != tc.wantLabel {
				t.Errorf("status label = %q, want %q", s.Status, tc.wantLabel)
			}
			if s.Service == "" || s.Version == "" {
				t.Error("a probe response must identify the service and build")
			}
		})
	}
}

// TestRoutesNotMountedWhenServiceIsAbsent asserts a binary that does not wire a service serves 404
// rather than 500 on that service's routes.
func TestRoutesNotMountedWhenServiceIsAbsent(t *testing.T) {
	t.Parallel()
	rt := newRouter(handlers.Deps{Webhooks: &fakeWebhooks{
		ingest: func(context.Context, appwebhook.InboundRequest) (*appwebhook.Accepted, error) {
			return &appwebhook.Accepted{}, nil
		},
	}})
	rec := do(rt, http.MethodGet, "/v1/payments/"+testPaymentID.String(), "", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a route this binary does not serve", rec.Code)
	}
	if rt.Has(http.MethodGet, httpapi.RouteGetPayment) {
		t.Error("the payment route was registered without a payment service")
	}
	if !rt.Has(http.MethodPost, httpapi.RouteReceiveWebhook) {
		t.Error("the webhook route was not registered despite the service being wired")
	}
}

// --- assertions ----------------------------------------------------------------------------------

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode apierror.Code) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, wantStatus, rec.Body)
	}
	if wantStatus >= 400 {
		var p httpapi.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatalf("error response is not a problem document: %v (%s)", err, rec.Body)
		}
		if ct := rec.Header().Get(httpapi.HeaderContentType); ct != httpapi.MediaProblem {
			t.Errorf("Content-Type = %q, want %q", ct, httpapi.MediaProblem)
		}
		if p.Status != wantStatus {
			t.Errorf("problem.status = %d, want %d", p.Status, wantStatus)
		}
		if p.TraceID == "" || len(p.TraceID) != 32 {
			t.Errorf("problem.traceId = %q, want 32 hex characters", p.TraceID)
		}
		if p.Type == "" || p.Title == "" || p.Category == "" {
			t.Errorf("problem is missing required fields: %+v", p)
		}
		if wantCode != "" && p.Code != string(wantCode) {
			t.Errorf("problem.code = %q, want %q", p.Code, wantCode)
		}
	}
	if wantStatus < 400 && wantStatus != http.StatusNotModified {
		if cc := rec.Header().Get(httpapi.HeaderCacheControl); cc != "no-store" {
			t.Errorf("Cache-Control = %q, want no-store", cc)
		}
	}
}

func mustJSON(t *testing.T, b []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(b, dst); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, b)
	}
}

func errCreate(err error) func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) {
	return func(context.Context, apppayment.CreateCommand) (*apppayment.Result, error) { return nil, err }
}

func okCapture() func(context.Context, apppayment.CaptureCommand) (*apppayment.Result, error) {
	return func(context.Context, apppayment.CaptureCommand) (*apppayment.Result, error) {
		p := newPayment()
		if err := p.MarkCaptured(p.Amount(), testClock); err != nil {
			panic(err)
		}
		return okResult(p), nil
	}
}

func okRefund(confirmed bool) func(context.Context, apppayment.RefundCommand) (*apppayment.Result, error) {
	return func(context.Context, apppayment.RefundCommand) (*apppayment.Result, error) {
		p := newPayment()
		if err := p.MarkCaptured(p.Amount(), testClock); err != nil {
			panic(err)
		}
		ref, err := p.AddRefund(p.Amount(), payment.RefundReasonRequestedByCustomer, "k1", testClock)
		if err != nil {
			panic(err)
		}
		if confirmed {
			if err := ref.MarkSubmitted("gw_ref", testClock.Now()); err != nil {
				panic(err)
			}
			if err := p.ConfirmRefund(ref.ID(), "gw_ref", testClock); err != nil {
				panic(err)
			}
		}
		return okResult(p), nil
	}
}

func healthUp() health.Response { return health.Response{Status: health.StatusUp} }
func healthDown() health.Response {
	return health.Response{
		Status: health.StatusDown,
		Checks: []health.Result{{Name: "postgres", Status: health.StatusDown, Error: "connection refused"}},
	}
}

var errSentinel = errorString("sentinel-internal-detail")

type errorString string

func (e errorString) Error() string { return string(e) }
