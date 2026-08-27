package middleware

import (
	"context"
	"net/http"
	"time"
)

// settleTimeout bounds a post-response idempotency settle.
//
// It is short because by this point the response has already been written and nobody is waiting:
// the only thing at stake is whether the record is marked before the process moves on. Two
// seconds is enough for a healthy Postgres round trip and short enough that a stalled store does
// not hold a worker for the duration of an incident.
const settleTimeout = 2 * time.Second

// settleContext returns a context that keeps the request's *values* — tenant, request id, trace
// — but not its cancellation, bounded by settleTimeout. The caller must call the returned cancel.
//
// Detaching is required rather than convenient. The idempotency record is settled after the
// handler returns, which is after net/http may already have observed the client disconnect and
// cancelled the request context. Settling on the cancelled context fails, the record stays
// IN_FLIGHT, and the client's next retry gets 409 IDEMPOTENT_REQUEST_IN_PROGRESS for the whole
// lease duration — for an operation that already completed. The values are kept because the
// store's own tenant scoping reads them.
//
// The timeout is what makes the detachment safe: a context with no cancellation and no deadline
// handed to a stalled store is a worker held forever, which is the leak the detachment was
// supposed to avoid.
func settleContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
}
