package resilience

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

// assertNoGoroutineLeaks snapshots the goroutine population at the start of a test and asserts,
// at its end, that nothing new is still running.
//
// Why this exists rather than a comment saying "we don't leak": the only two types in this
// package that ever start a goroutine are BreakerRegistry and BulkheadRegistry, and both start
// theirs conditionally and stop it in Close. That is exactly the shape of contract that rots —
// somebody adds a ticker, forgets the stop channel, and the symptom is a pod whose goroutine
// count climbs over days until it is OOM-killed with no obvious cause. A leak check that runs
// on every test of every background-work path turns that into a red test on the same afternoon.
//
// It tolerates the runtime's own transient goroutines by retrying for a bounded window: a
// goroutine that is on its way out disappears within milliseconds, and one that is leaked does
// not disappear at all.
func assertNoGoroutineLeaks(t *testing.T) {
	t.Helper()
	before := goroutineSet()

	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		var leaked []string
		for {
			runtime.GC()
			leaked = nil
			for id, stack := range goroutineSet() {
				if _, existed := before[id]; existed {
					continue
				}
				if isRuntimeNoise(stack) {
					continue
				}
				leaked = append(leaked, stack)
			}
			if len(leaked) == 0 {
				return
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Errorf("goroutine leak: %d goroutine(s) still running after the test\n%s",
			len(leaked), strings.Join(leaked, "\n---\n"))
	})
}

// goroutineSet returns the live goroutines keyed by their id, with their stacks as values.
func goroutineSet() map[string]string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]string{}
	for _, block := range strings.Split(string(buf), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		header, _, ok := strings.Cut(block, "\n")
		if !ok {
			continue
		}
		// "goroutine 42 [running]:" — the numeric id is the second field.
		fields := strings.Fields(header)
		if len(fields) < 2 {
			continue
		}
		out[fields[1]] = block
	}
	return out
}

// isRuntimeNoise filters out every goroutine that is not ours.
//
// The filter is deliberately narrow: a goroutine counts as a leak only if this package appears
// somewhere in its stack, which covers both a function of ours running and a
// "created by …/resilience.…" frame for one parked in a channel receive. Anything else — the
// GC's workers, the testing package's machinery, a parallel test's goroutines — belongs to
// somebody else, and asserting on it would make this helper fail for reasons that have nothing
// to do with the code under test.
func isRuntimeNoise(stack string) bool {
	return !strings.Contains(stack, "infrastructure/resilience.")
}
