package domain

import "errors"

// Sentinel domain errors. The API layer maps these to specific HTTP status codes (see
// internal/api/handlers.go) — the domain layer never knows about HTTP, keeping the business logic
// transport-agnostic and independently testable.
var (
	ErrInvalidRequest       = errors.New("invalid request")
	ErrAccountNotFound      = errors.New("account not found")
	ErrAccountFrozen        = errors.New("account is frozen")
	ErrCurrencyMismatch     = errors.New("currency mismatch between accounts and payment")
	ErrInsufficientFunds    = errors.New("insufficient funds")
	ErrIdempotencyConflict  = errors.New("idempotency key reused with a different request body")
	ErrPaymentNotFound      = errors.New("payment not found")
	ErrConcurrentModification = errors.New("concurrent modification, retry")
)
