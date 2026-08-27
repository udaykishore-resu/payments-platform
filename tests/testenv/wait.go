package testenv

import (
	"context"
	"testing"
	"time"
)

// DefaultPollInterval is how often Eventually re-checks. It is short enough that a test does not
// pay a whole interval of latency for a condition that was already true, and long enough that a
// condition involving a database round trip is not being asked twenty times per millisecond.
const DefaultPollInterval = 25 * time.Millisecond

// Eventually polls cond until it returns true or the budget expires.
//
// This is the *only* sanctioned way to wait for something in this suite. A bare time.Sleep is a
// bet that an unrelated machine will be no slower than the one the test was written on, and it
// loses that bet on the day CI is busy — producing a failure that looks like a product bug and is
// re-run away. Eventually turns the same situation into a message naming the condition and the
// budget it exceeded, which is a bug report rather than a coin toss.
//
// Note the asymmetry with a Sleep: a condition that becomes true immediately costs nothing here,
// so tightening a budget is free and only ever improves the failure signal.
func Eventually(t testing.TB, budget time.Duration, describe string, cond func() bool) {
	t.Helper()
	if EventuallyErr(budget, DefaultPollInterval, cond) == nil {
		return
	}
	t.Fatalf("timed out after %s waiting for: %s", budget, describe)
}

// EventuallyErr is Eventually without a testing.TB, for use inside harness code.
func EventuallyErr(budget, interval time.Duration, cond func() bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	if cond() {
		return nil
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if cond() { // one last look: the deadline may have fired between ticks
				return nil
			}
			return ctx.Err()
		case <-tick.C:
			if cond() {
				return nil
			}
		}
	}
}

// Consistently asserts that cond holds for the whole window, sampling at interval.
//
// The complement of Eventually, and the one the chaos suite needs: a steady-state hypothesis that
// is only checked after the fault has healed cannot distinguish "the invariant held" from "the
// invariant broke and the system corrected itself" — and the second is precisely the shape of the
// bug that later surfaces as an unexplained duplicate.
func Consistently(t testing.TB, window time.Duration, describe string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(window)
	tick := time.NewTicker(DefaultPollInterval)
	defer tick.Stop()
	for {
		if !cond() {
			t.Fatalf("condition stopped holding during the observation window: %s", describe)
		}
		if !time.Now().Before(deadline) {
			return
		}
		<-tick.C
	}
}

// Deadline returns a context bounded by the test's own budget.
func Deadline(t testing.TB, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}
