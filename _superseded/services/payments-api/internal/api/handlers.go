package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/example/payments-platform/services/payments-api/internal/domain"
	"github.com/example/payments-platform/services/payments-api/internal/middleware"
	"github.com/example/payments-platform/services/payments-api/internal/observability"
	"github.com/example/payments-platform/services/payments-api/internal/repository"
)

const maxRequestBodyBytes = 1 << 16 // 64KiB — a payment request body is tiny; this is a cheap
// backstop against a malformed/malicious oversized body doing unnecessary work before validation.

type PaymentService interface {
	Create(ctx context.Context, in domain.CreatePaymentInput) (domain.Payment, bool, error)
	Get(ctx context.Context, id string) (domain.Payment, error)
}

type Handlers struct {
	service PaymentService
	logger  *slog.Logger
}

func NewHandlers(service PaymentService, logger *slog.Logger) *Handlers {
	return &Handlers{service: service, logger: logger}
}

// CreatePayment handles POST /v1/payments. See docs/02-architecture.md, "Request Flow — Create
// Payment" for the full step-by-step behind service.Create.
func (h *Handlers) CreatePayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := observability.WithTraceContext(ctx, h.logger)

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key header is required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body")
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "payload_too_large", "request body too large")
		return
	}

	var req CreatePaymentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "malformed JSON body")
		return
	}

	clientID := "unknown"
	if claims, ok := middleware.ClientFromContext(ctx); ok {
		clientID = claims.ClientID
	}

	input := domain.CreatePaymentInput{
		IdempotencyKey:  idempotencyKey,
		RequestHash:     repository.HashRequestBody(body),
		SourceAccountID: req.SourceAccountID,
		DestAccountID:   req.DestAccountID,
		Amount:          domain.Money{AmountMinor: req.AmountMinor, Currency: req.Currency},
		ClientID:        clientID,
	}

	payment, idempotentHit, err := h.service.Create(ctx, input)
	if err != nil {
		h.writeDomainError(w, logger, err)
		return
	}

	status := http.StatusCreated
	if idempotentHit {
		// Same status code either way is deliberate per FR-2: a client replaying a request
		// should not be able to distinguish "first time" from "retry" by status code alone —
		// both mean "here is the authoritative, committed result."
		status = http.StatusOK
	}
	writeJSON(w, status, toPaymentResponse(payment))
}

// GetPayment handles GET /v1/payments/{id}.
func (h *Handlers) GetPayment(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	logger := observability.WithTraceContext(ctx, h.logger)

	id := r.PathValue("id")
	payment, err := h.service.Get(ctx, id)
	if err != nil {
		h.writeDomainError(w, logger, err)
		return
	}
	writeJSON(w, http.StatusOK, toPaymentResponse(payment))
}

func (h *Handlers) writeDomainError(w http.ResponseWriter, logger *slog.Logger, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, domain.ErrAccountNotFound):
		writeError(w, http.StatusUnprocessableEntity, "account_not_found", err.Error())
	case errors.Is(err, domain.ErrAccountFrozen):
		writeError(w, http.StatusUnprocessableEntity, "account_frozen", err.Error())
	case errors.Is(err, domain.ErrCurrencyMismatch):
		writeError(w, http.StatusUnprocessableEntity, "currency_mismatch", err.Error())
	case errors.Is(err, domain.ErrInsufficientFunds):
		writeError(w, http.StatusUnprocessableEntity, "insufficient_funds", err.Error())
	case errors.Is(err, domain.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "idempotency_key_conflict", err.Error())
	case errors.Is(err, domain.ErrPaymentNotFound):
		writeError(w, http.StatusNotFound, "payment_not_found", err.Error())
	default:
		logger.Error("unhandled service error", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_server_error", "an unexpected error occurred")
	}
}

func toPaymentResponse(p domain.Payment) PaymentResponse {
	return PaymentResponse{
		ID:              p.ID,
		SourceAccountID: p.SourceAccountID,
		DestAccountID:   p.DestAccountID,
		AmountMinor:     p.Amount.AmountMinor,
		Currency:        p.Amount.Currency,
		Status:          string(p.Status),
		CreatedAt:       p.CreatedAt,
	}
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message})
}
