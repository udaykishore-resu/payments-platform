package resilience

import (
	"context"
	"sync"
	"time"
)

// Clock is the package's only source of time and its only way to wait.
//
// Every state machine here is timing-dependent: the breaker's rolling window, its cool-down
// doubling, the retry budget's refill, the shedder's hysteresis, the adaptive limiter's
// sampling windows. A test that drives those with real sleeps is a test that is slow when the
// machine is idle and flaky when it is not, and docs/testing.md is explicit that a flaky test
// in the money path blocks the release. Injecting the clock is what makes "the cool-down
// doubles from 30 s to 60 s and caps at 5 min" a sub-millisecond assertion instead of an
// eleven-minute one.
//
// Implementations must be safe for concurrent use.
type Clock interface {
	// Now returns the current time in UTC.
	Now() time.Time
	// Sleep blocks for d or until ctx is done, whichever happens first, returning ctx.Err()
	// in the latter case. A non-positive d returns immediately with ctx.Err().
	//
	// It is a method on Clock rather than a free function because a retry that sleeps on the
	// wall clock while its budget accounting runs on an injected clock is a retry whose tests
	// prove nothing.
	Sleep(ctx context.Context, d time.Duration) error
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now().UTC() }

func (systemClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	// The timer is stopped on every path. An unstopped timer holds its runtime entry until it
	// fires, which for a 30-minute DLQ backoff cap is thirty minutes of retained memory per
	// abandoned wait.
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SystemClock returns the production clock: wall time in UTC, real waits.
//
// It is a function rather than an exported variable so no caller can reassign the platform's
// notion of time from another package.
func SystemClock() Clock { return systemClock{} }

// ManualClock is a Clock whose time only moves when a test moves it.
//
// Sleep does not block: it records the requested duration, runs the OnSleep hook if one is
// installed, and advances the clock by exactly d. That is what lets a test assert "the retry
// slept 100 ms then 200 ms then gave up" in microseconds, and lets a test cancel a context
// *during* a backoff by cancelling inside the hook.
//
// Safe for concurrent use.
type ManualClock struct {
	mu      sync.Mutex
	now     time.Time
	slept   []time.Duration
	onSleep func(d time.Duration)
}

// NewManualClock returns a ManualClock positioned at t. A zero t is replaced with a fixed,
// arbitrary instant so that durations subtracted from it never underflow into the year 1.
func NewManualClock(t time.Time) *ManualClock {
	if t.IsZero() {
		t = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	}
	return &ManualClock{now: t.UTC()}
}

// Now returns the clock's current instant.
func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d. A non-positive d is ignored: time in this package is
// only ever allowed to move forward, because every window and cool-down computation assumes it.
func (c *ManualClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Sleep records d, runs the OnSleep hook, and advances the clock by d. It returns ctx.Err() if
// the context is already done, or becomes done inside the hook — which is precisely how a test
// simulates a cancellation landing in the middle of a backoff.
func (c *ManualClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.slept = append(c.slept, d)
	hook := c.onSleep
	c.mu.Unlock()

	if hook != nil {
		hook(d)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

// OnSleep installs a hook invoked at the start of every Sleep, before the clock advances.
// Passing nil removes it.
func (c *ManualClock) OnSleep(fn func(d time.Duration)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onSleep = fn
}

// Slept returns a copy of every duration passed to Sleep, in order. A copy, not the live slice,
// so an assertion cannot mutate the record it is asserting on.
func (c *ManualClock) Slept() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.slept...)
}

// orSystem returns clk, or the system clock when clk is nil, so every config struct in this
// package can leave Clock unset and still work in production.
func orSystem(clk Clock) Clock {
	if clk == nil {
		return SystemClock()
	}
	return clk
}
