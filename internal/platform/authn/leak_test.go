package authn_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/authn"
)

// TestJWKSStartStopDoesNotLeakGoroutines exercises the one place in this package that starts
// background work.
//
// Why it is worth a test rather than a comment saying "we stop the goroutine": the contract —
// start conditionally, stop in Stop, tolerate Stop-without-Start and Start-after-Stop — is
// exactly the shape that rots. Somebody adds a second ticker, forgets a stop channel, and the
// symptom is a pod whose goroutine count climbs over days until it is OOM-killed with no obvious
// cause. A hundred start/stop cycles turn that into a red test on the same afternoon.
func TestJWKSStartStopDoesNotLeakGoroutines(t *testing.T) {
	rk, _, _ := keys(t)
	before := settledGoroutines()

	for i := 0; i < 100; i++ {
		j := authn.NewJWKS(&staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk))}, authn.JWKSConfig{
			Clock:              shared.SystemClock{},
			RefreshInterval:    time.Hour, // never fires; we are testing the lifecycle, not the work
			MinRefreshInterval: time.Nanosecond,
		})
		j.Register(testIssuer, testJWKSURL)
		j.Start(context.Background())
		j.Stop()
	}
	// A cache that was never started must also not leave anything behind.
	for i := 0; i < 100; i++ {
		j := authn.NewJWKS(&staticFetcher{}, authn.JWKSConfig{})
		j.Stop()
	}
	// And one cancelled through its context rather than through Stop.
	ctx, cancel := context.WithCancel(context.Background())
	j := authn.NewJWKS(&staticFetcher{body: jwksDocument(rsaJWK("rsa-1", rk))}, authn.JWKSConfig{
		Clock: shared.SystemClock{}, RefreshInterval: time.Millisecond, MinRefreshInterval: time.Nanosecond,
	})
	j.Register(testIssuer, testJWKSURL)
	j.Start(ctx)
	cancel()

	after := settledGoroutines()
	// A small tolerance for the runtime's own transient goroutines; a genuine leak here would be
	// two hundred, not two.
	if after > before+3 {
		t.Fatalf("goroutines grew from %d to %d across 200 lifecycle cycles", before, after)
	}
	j.Stop()
}

// settledGoroutines waits for the count to stop moving, so a goroutine on its way out is not
// mistaken for a leak.
func settledGoroutines() int {
	deadline := time.Now().Add(2 * time.Second)
	last := runtime.NumGoroutine()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
		n := runtime.NumGoroutine()
		if n == last {
			return n
		}
		last = n
	}
	return last
}
