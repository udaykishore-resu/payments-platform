package middleware

import (
	"context"
	"errors"
	"sync"
	"time"
)

type BreakerState int

const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

var ErrCircuitOpen = errors.New("circuit breaker open")

// CircuitBreaker is a small, dependency-free implementation of the classic pattern: after
// `failureThreshold` consecutive failures, it opens and fails fast for `openDuration` instead of
// letting every request hang against (or hammer) a struggling dependency — see
// docs/04-failure-recovery-design.md, "Dependency slow/unavailable" and
// docs/08-runbook.md section 3.
//
// Deliberately hand-rolled rather than pulling in a third-party breaker library: the state
// machine is small enough that owning it directly is less risk than an opaque dependency for
// something this central to the failure-handling story, and it keeps this reference
// implementation dependency-light.
type CircuitBreaker struct {
	name             string
	failureThreshold int
	openDuration     time.Duration

	mu              sync.Mutex
	state           BreakerState
	consecutiveFail int
	openedAt        time.Time

	onStateChange func(name string, s BreakerState)
}

func NewCircuitBreaker(name string, failureThreshold int, openDuration time.Duration, onStateChange func(string, BreakerState)) *CircuitBreaker {
	return &CircuitBreaker{
		name:             name,
		failureThreshold: failureThreshold,
		openDuration:     openDuration,
		onStateChange:    onStateChange,
	}
}

// Execute runs fn if the breaker allows it, and records the outcome. When the breaker is open and
// the cool-down period hasn't elapsed, it fails fast with ErrCircuitOpen without calling fn at
// all — this is what stops a struggling database from also exhausting this service's own
// goroutines/connections while it's down.
func (b *CircuitBreaker) Execute(ctx context.Context, fn func(context.Context) error) error {
	if !b.allow() {
		return ErrCircuitOpen
	}
	err := fn(ctx)
	b.recordResult(err == nil)
	return err
}

func (b *CircuitBreaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case StateOpen:
		if time.Since(b.openedAt) >= b.openDuration {
			b.setState(StateHalfOpen)
			return true // allow exactly one trial request through
		}
		return false
	default:
		return true
	}
}

func (b *CircuitBreaker) recordResult(success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if success {
		b.consecutiveFail = 0
		if b.state != StateClosed {
			b.setState(StateClosed)
		}
		return
	}

	b.consecutiveFail++
	if b.state == StateHalfOpen || b.consecutiveFail >= b.failureThreshold {
		b.openedAt = time.Now()
		b.setState(StateOpen)
	}
}

func (b *CircuitBreaker) setState(s BreakerState) {
	b.state = s
	if b.onStateChange != nil {
		b.onStateChange(b.name, s)
	}
}

func (b *CircuitBreaker) State() BreakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
