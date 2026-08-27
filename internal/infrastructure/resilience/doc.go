// Package resilience implements the platform's resilience toolkit: the timeout cascade, retry
// with budgets, exponential backoff with full jitter, circuit breakers, bulkheads, rate
// limiters, adaptive concurrency limits and priority-aware load shedding.
//
// Every parameter in this package is derived, not chosen. The derivations live in
// docs/failure-handling.md §2 and docs/spec/00-design-baseline.md §10, §12 and §24, and each is
// restated at the point in the code where it is applied — a magic number whose reasoning lives
// only in a document is a magic number that will be "tuned" during an incident by somebody who
// has not read the document.
//
// The package is a generic infrastructure primitive library. It imports the standard library
// and pkg/apierror and nothing else. It deliberately knows nothing about payments, gateways or
// merchants: the only payment-domain knowledge encoded here is in the ordering of
// PriorityClass, which exists because "money-out survives longer than money-in" is a property
// of the toolkit, not of a caller (see shed.go).
//
// Two rules from the baseline govern the whole file set and are worth stating once:
//
//  1. No timer may fail a payment (baseline §12.3). Nothing in this package converts a timeout
//     into a terminal failure; a timeout produces an error whose apierror code is
//     GATEWAY_TIMEOUT with Retryable=false, which is the platform's way of saying "the outcome
//     is unknown, reconcile — do not guess".
//  2. Fail static, not fail open (baseline §15). Every fallback in this package (the rate
//     limiter's local bucket, the breaker's half-open probe, the shedder's reserved P0
//     capacity) degrades toward a bounded, defined behaviour, never toward "allow everything".
//
// Concurrency: every exported type in this package documents its own safety guarantee. Unless
// a type says otherwise, its methods are safe for concurrent use by multiple goroutines.
//
// Goroutines: only two types in this package ever start one — BreakerRegistry and
// BulkheadRegistry, and only when a SweepInterval is configured. Both own their goroutine and
// both stop it in Close. Everything else advances its state machines lazily, on the calling
// goroutine, which is what makes the whole package testable with a ManualClock and free of
// leaks by construction.
package resilience
