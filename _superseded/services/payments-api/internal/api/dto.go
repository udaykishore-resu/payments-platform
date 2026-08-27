// Package api is the HTTP transport layer: request/response DTOs, validation, and handlers. It
// translates between the wire format and the transport-agnostic domain/service layer, and is the
// only place that knows about HTTP status codes.
package api

import "time"

// CreatePaymentRequest is the wire format for POST /v1/payments. Deliberately a distinct type
// from domain.Payment (never bind raw JSON directly onto domain/DB structs — see
// docs/05-security-architecture.md, "mass assignment").
type CreatePaymentRequest struct {
	SourceAccountID string `json:"source_account_id"`
	DestAccountID   string `json:"dest_account_id"`
	AmountMinor     int64  `json:"amount_minor"`
	Currency        string `json:"currency"`
}

type PaymentResponse struct {
	ID              string    `json:"id"`
	SourceAccountID string    `json:"source_account_id"`
	DestAccountID   string    `json:"dest_account_id"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
