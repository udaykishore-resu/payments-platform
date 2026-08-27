package payment

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// TestRiskEvaluationLeaksNoGoroutines.
//
// The risk assessor is the only place in this layer that fans out, and a leak here is the kind
// that never shows up in a unit test and strangles a process in production: the scorer is the one
// dependency with a deadline shorter than the request's, so an implementation that returned as
// soon as *its* deadline fired — rather than waiting out the fan-out — would leave a goroutine
// per payment writing into a struct nobody reads.
//
// The assertion is a goroutine census either side of a batch of evaluations, with the scorer
// deliberately timing out on every one of them.
func TestRiskEvaluationLeaksNoGoroutines(t *testing.T) {
	// Not parallel: the census counts every goroutine in the process, so a sibling test running
	// concurrently would make the count meaningless.
	assessor := NewRiskAssessor(RiskAssessorDeps{
		Velocity:        apptest.NewVelocity(),
		Blocklists:      failingBlocklist{},
		Scorer:          &apptest.Scorer{Delay: 50 * time.Millisecond},
		ScorerTimeout:   time.Millisecond,
		TRAScoreCeiling: 30,
		Clock:           apptest.NewClock(testEpoch),
	})

	before := settledGoroutines()
	for i := 0; i < 50; i++ {
		if _, err := assessor.Evaluate(context.Background(), RiskInput{
			Policy:    risk.Policy{MaxTransactionAmount: money.MustNew(100000, "EUR")},
			TenantID:  testTenant,
			Amount:    mustEUR(1000),
			Method:    shared.MethodCard,
			MethodRef: dpayment.PaymentMethodReference{Token: "tok"},
			Customer:  dpayment.CustomerReference{Country: "DE"},
			Merchant:  defaultSnapshot(), Now: testEpoch,
		}); err != nil {
			t.Fatalf("Evaluate %d: %v", i, err)
		}
	}
	after := settledGoroutines()

	// A small tolerance rather than exact equality: the runtime's own workers come and go, and a
	// leak of the kind this test exists to catch would be fifty, not two.
	if after > before+2 {
		t.Fatalf("goroutines went from %d to %d across 50 evaluations; the fan-out leaks", before, after)
	}
}

// settledGoroutines gives the runtime a moment to reap finished goroutines before counting.
//
// Without it the census races the scheduler and reports a phantom leak on a busy machine, which
// is the classic way a leak test earns itself a `t.Skip`.
func settledGoroutines() int {
	prev := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		time.Sleep(2 * time.Millisecond)
		runtime.Gosched()
		n := runtime.NumGoroutine()
		if n == prev {
			return n
		}
		prev = n
	}
	return prev
}
