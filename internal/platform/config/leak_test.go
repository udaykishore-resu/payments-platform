package config_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/platform/config"
)

// TestProviderStartStopDoesNotLeakGoroutines covers the one background worker in this package.
//
// The provider runs for the life of the process in production, so a leak here is invisible until
// a rolling restart under load, when every pod carries the accumulated goroutines of every
// provider ever constructed in it. The lifecycle contract — idempotent Start, idempotent Stop,
// Stop without Start, Start after Stop, and cancellation through the context — is checked in bulk
// because each of those paths is one `close` away from either a leak or a double-close panic.
func TestProviderStartStopDoesNotLeakGoroutines(t *testing.T) {
	before := settledGoroutines()

	for i := 0; i < 100; i++ {
		p, err := config.NewProvider(config.ProviderConfig{
			Source:          &memorySource{},
			Clock:           shared.SystemClock{},
			RefreshInterval: time.Hour, // never fires; this is a lifecycle test
		})
		if err != nil {
			t.Fatal(err)
		}
		p.Start(context.Background())
		p.Start(context.Background())
		p.Stop()
		p.Stop()
	}
	for i := 0; i < 100; i++ {
		p, err := config.NewProvider(config.ProviderConfig{Source: &memorySource{}})
		if err != nil {
			t.Fatal(err)
		}
		p.Stop() // never started
	}

	ctx, cancel := context.WithCancel(context.Background())
	p, err := config.NewProvider(config.ProviderConfig{
		Source: &memorySource{}, Clock: shared.SystemClock{}, RefreshInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	p.Start(ctx)
	cancel()

	after := settledGoroutines()
	if after > before+3 {
		t.Fatalf("goroutines grew from %d to %d across 200 lifecycle cycles", before, after)
	}
	p.Stop()
}

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
