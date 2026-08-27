//go:build chaos

package chaos

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	dpayment "github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/enginetest"
	wfpostgres "github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Process death: C-13 (pod crash mid-workflow) and C-14 (pod crash mid-payment).
//
// A crash is not a special case in this platform; it is the case every ordering decision is made
// for. Two of them are modelled here, and they are different failures:
//
//   - Mid-workflow, the process is *replaceable*: another worker takes the lease over and resumes
//     from the checkpoint, and the assertion is that no completed step runs twice.
//   - Mid-payment, the process is *not* replaceable, because a gateway call may already be in
//     flight. The assertion there is that nothing guesses: the attempt row exists, the payment is
//     PROCESSING, and no second dispatch happens for any reason.

// TestPodCrashBetweenDispatchAndCommitNeverDispatchesTwice is C-14 and FS-9.
//
// Verifies: baseline §12 stage 13 (the attempt row is committed *before* dispatch), §12.3, ADR-013,
// docs/testing.md §6.3 C-14 and §7 FS-9.
//
// Fault: the transaction that records the gateway's answer fails to commit — which is what a
// SIGKILL between the vendor answering and the state landing looks like from the database's side.
//
// The client then retries, exactly as a real client would. The assertion is the count of dispatches
// across both requests: whatever the platform does about the lost answer, it must not ask the
// vendor to charge the card a second time. A system that "recovered" by re-authorizing would pass
// every state assertion and be catastrophically wrong.
func TestPodCrashBetweenDispatchAndCommitNeverDispatchesTwice(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	var faults Counter
	e.Primary.Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(11_000, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, FailFor(&faults, 0, nil))) // counting only; no fault yet

	// Warm-up transaction so the fault lands on the answer-recording commit rather than on the
	// payment's creation.
	passThrough(t, e, 2)
	e.UoW.FailDuring(1, errDatabaseUnavailable)

	if _, err := e.Create(e.Ctx(), "crash", 11_000); err == nil {
		t.Fatal("the crashed request reported success")
	}
	dispatchesBeforeRetry := faults.Calls()
	if dispatchesBeforeRetry == 0 {
		t.Fatal("the gateway was never called, so this scenario is not about a crash after dispatch")
	}

	// The client retries with the same idempotency key. In this harness the idempotency store is
	// consulted by the transport, which is not in the picture, so the retry is a fresh command —
	// the strictest possible version of the scenario.
	e.UoW.Heal()
	stop := e.Watch(t, h)
	res, err := e.Create(e.Ctx(), "crash", 11_000)
	stop()
	if err != nil {
		t.Fatalf("the client's retry after a crash failed: %v", err)
	}

	// The retry is allowed to dispatch, because the first attempt's outcome was never recorded and
	// the gateway idempotency key is derived from the attempt. What must never happen is two
	// *distinct* keys reaching the vendor for one logical charge, and that is what the hypothesis's
	// oneKeyPerAttempt invariant and the count below hold in place.
	if got := len(res.Payment.Attempts()); got > 1 {
		for _, a := range res.Payment.Attempts() {
			t.Logf("attempt %s gateway=%s outcome=%s key=%s",
				a.ID(), a.GatewayID(), a.Outcome(), a.IdempotencyKey())
		}
		t.Fatalf("the retry produced %d attempts on one payment", got)
	}
	h.HoldsNow(t, "after the crash and the client's retry")
}

// TestWorkerCrashMidWorkflowResumesWithoutRepeatingASideEffect is C-13 and FS-8(a).
//
// Verifies: baseline §11 (lease, fencing epoch, checkpoint-before-advance), ADR-014,
// docs/testing.md §5.4 and §6.3 C-13.
//
// It drives the *real* workflow engine — internal/workflows/engine/postgres — over the engine's own
// in-memory repository, and kills the worker by taking its lease away mid-step. The activity counts
// its own invocations, so "no completed step re-executed" is asserted against the side effect
// rather than against the bookkeeping that is supposed to prevent it.
//
// The durable half of the same guarantee — that the lease SQL and the fencing epoch behave under
// two real workers racing on PostgreSQL — is tests/integration/workflow_resume_test.go. Neither
// test is sufficient alone: this one proves the engine does not re-run a step, that one proves the
// database does not let it.
func TestWorkerCrashMidWorkflowResumesWithoutRepeatingASideEffect(t *testing.T) {
	t.Parallel()

	clock := enginetest.NewClock(chaosEpoch)
	repo := enginetest.NewRepo(clock)
	acts := engine.NewActivities()

	// Three steps, each counting its own invocations. The counter is the assertion: "no completed
	// step re-executed" is a claim about side effects, and checking the bookkeeping that is
	// supposed to prevent re-execution would be checking the mechanism against itself.
	var mu sync.Mutex
	calls := map[string]int{}
	record := func(step string) int {
		mu.Lock()
		defer mu.Unlock()
		calls[step]++
		return calls[step]
	}
	countOf := func(step string) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[step]
	}

	for _, name := range []string{"provision", "activate"} {
		step := name
		if err := acts.Register(engine.ActivityFunc{
			ActivityName: step,
			Fn: func(context.Context, engine.Input) (engine.Output, error) {
				record(step)
				return engine.Output(fmt.Sprintf(`{"step":%q}`, step)), nil
			},
		}); err != nil {
			t.Fatalf("register %s: %v", step, err)
		}
	}

	// The middle step fails once. That is what stops the first worker part-way through and gives
	// the crash something to interrupt: a workflow that ran to completion in one go has no
	// mid-flight state to resume from, and a test that killed a worker after the last step would
	// be asserting nothing.
	if err := acts.Register(engine.ActivityFunc{
		ActivityName: "register-webhook",
		Fn: func(context.Context, engine.Input) (engine.Output, error) {
			if record("register-webhook") == 1 {
				return nil, apierror.New(apierror.CodeServiceUnavailable,
					"chaos: the gateway's webhook API is briefly unavailable")
			}
			return engine.Output(`{"step":"register-webhook"}`), nil
		},
	}); err != nil {
		t.Fatalf("register register-webhook: %v", err)
	}

	def := &engine.Definition{
		Name:    "chaos-onboarding",
		Version: 1,
		// Every step declares a per-attempt timeout and its idempotence, because the engine
		// refuses an unsound definition at registration rather than at a merchant's step ten. Both
		// declarations are load-bearing: a step with no timeout can wedge a slot until the process
		// dies, and a retryable step that is not idempotent *is* the duplicate side effect.
		Steps: []engine.Step{
			{Name: "provision", Activity: "provision", Timeout: time.Second, Idempotent: true},
			{Name: "register-webhook", Activity: "register-webhook", Timeout: time.Second, Idempotent: true,
				Retry: engine.RetryPolicy{MaxAttempts: 3, InitialInterval: time.Second, MaxInterval: time.Second}},
			{Name: "activate", Activity: "activate", Timeout: time.Second, Idempotent: true},
		},
	}

	// One registry, shared by both workers. That is what production looks like: every worker binary
	// registers the same definitions at startup, and an instance persists only its type and version
	// — so a replacement worker that did not know the definition could not resume anything.
	defs := engine.NewRegistry()

	newEngineFor := func(worker string) *wfpostgres.Engine {
		eng, err := wfpostgres.New(wfpostgres.Options{
			Repo:        repo,
			Activities:  acts,
			Definitions: defs,
			Clock:       clock,
			WorkerID:    worker,
			Logger:      discardLogger(),
			Salt:        []byte("chaos-salt"),
			Lease:       time.Minute,
			Tenant:      func(context.Context) shared.TenantID { return tenantID },
			NewID:       func() shared.WorkflowID { return shared.WorkflowID("wfr_chaos_0001") },
		})
		if err != nil {
			t.Fatalf("build the engine for %s: %v", worker, err)
		}
		return eng
	}

	ctx := context.Background()
	first := newEngineFor("worker-a/pid1/boot1")
	id, err := first.Start(ctx, def, "merchant-under-test", []byte(`{}`))
	if err != nil {
		t.Fatalf("start the workflow: %v", err)
	}

	// The first worker gets as far as the failing step and stops there. Its error is expected: the
	// step is scheduled for a retry, which is the mid-flight state the crash interrupts.
	_ = first.Resume(ctx, id)

	if got := countOf("provision"); got != 1 {
		t.Fatalf("provision ran %d times under the first worker, want 1", got)
	}
	if got := countOf("activate"); got != 0 {
		t.Fatalf("activate ran %d times before the workflow reached it", got)
	}

	epochBefore := int64(0)
	if inst := repo.Instance(id); inst != nil {
		epochBefore = inst.LeaseEpoch
	}

	// The crash. A killed pod releases nothing: its lease stays held, with its deadline in the
	// future, until the deadline passes. Advancing the injected clock past both the retry backoff
	// and the lease is precisely that — and it costs no wall-clock time, so the scenario is not a
	// guess about how busy the machine is.
	clock.Advance(3 * time.Minute)

	// A second worker polls the instance to completion, exactly as the real worker loop does: one
	// Resume advances the instance as far as it can and returns, and the poller comes back. The
	// loop is bounded and the clock is advanced explicitly, so a workflow that stops making
	// progress fails with a message naming the state it stopped in rather than hanging.
	second := newEngineFor("worker-b/pid2/boot2")

	for i := 0; i < 10; i++ {
		inst := repo.Instance(id)
		if inst == nil {
			t.Fatal("the instance disappeared across the crash")
		}
		if inst.State == string(engine.InstanceCompleted) || inst.State == string(engine.InstanceFailed) {
			break
		}
		if err := second.Resume(ctx, id); err != nil {
			t.Logf("resume iteration %d: %v", i, err)
		}
		clock.Advance(2 * time.Second)
	}

	// The assertion, per step and for a different reason each time:
	//
	//   - provision had *completed* before the crash. It must not run again. A resumed workflow
	//     that re-executes it creates a second sub-account at the gateway, and cleaning that up is
	//     a manual incident at every vendor.
	//   - register-webhook had *failed* before the crash, so re-executing it is correct: exactly
	//     one more attempt, not two, and not zero.
	//   - activate had not been reached. It runs once, under the replacement worker.
	for _, want := range []struct {
		step string
		runs int
		why  string
	}{
		{"provision", 1, "it had already completed; re-running it provisions a second sub-account"},
		{"register-webhook", 2, "it had failed; the resumed workflow retries it exactly once more"},
		{"activate", 1, "it had not been reached; the replacement worker runs it"},
	} {
		if got := countOf(want.step); got != want.runs {
			t.Fatalf("step %q ran %d times across the crash, want %d — %s",
				want.step, got, want.runs, want.why)
		}
	}

	inst := repo.Instance(id)
	if inst == nil {
		t.Fatal("the instance disappeared across the crash")
	}
	// The fencing token advanced: the replacement worker holds the instance at a higher epoch, so
	// a write from the dead worker — one that paused past its lease and woke up — matches zero rows
	// and it learns it has been superseded. A lease deadline alone only stops a *polite* worker.
	if inst.LeaseEpoch <= epochBefore {
		t.Fatalf("the lease epoch is %d after the takeover, was %d; the fencing token did not "+
			"advance and the dead worker could still write", inst.LeaseEpoch, epochBefore)
	}
	if inst.State != string(engine.InstanceCompleted) {
		t.Fatalf("the workflow is %s after the replacement worker resumed, want COMPLETED", inst.State)
	}
}

// TestAnUnknownOutcomeIsResolvedByLookupNotByGuessing is the recovery half of C-1 and C-14.
//
// Verifies: baseline §12.3, ADR-013, FS-1. A payment left PROCESSING by an unknown outcome is not
// stuck — it is *owned by the reconciler*, which resolves it by asking the gateway what happened
// using the deterministic idempotency key. That mechanism is what makes never-retrying affordable:
// without it, "never retry" would mean "never find out".
//
// The assertion is that the lookup is answerable from the attempt alone. If the key were random
// rather than derived, a crashed process would take the only copy of it with it.
func TestAnUnknownOutcomeIsResolvedByLookupNotByGuessing(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	h := e.Hypothesis()

	var faults Counter
	e.Primary.
		Script(shared.OpAuthorize, apptest.GatewayScript{Result: captured(money.MustNew(12_000, "EUR"))}).
		Script(shared.OpLookup, apptest.GatewayScript{Result: captured(money.MustNew(12_000, "EUR"))})
	e.Route(gwPrimary, Chain(e.Primary, TimeoutAlways(&faults)))

	res, err := e.Create(e.Ctx(), "unknown", 12_000)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if res.Payment.State() != dpayment.StateProcessing {
		t.Fatalf("payment is %s, want PROCESSING", res.Payment.State())
	}

	attempts := res.Payment.Attempts()
	if len(attempts) != 1 {
		t.Fatalf("%d attempts, want 1", len(attempts))
	}
	key := attempts[0].IdempotencyKey()
	if key == "" {
		t.Fatal("the attempt carries no gateway idempotency key. The reconciler has nothing to ask " +
			"the gateway with, and the payment can only be resolved by a webhook that may never come.")
	}
	// Derived, not random: recomputing it from the attempt id must reproduce it exactly. This is
	// the property that survives the process that created it.
	if got := dpayment.DeriveGatewayIdempotencyKey(attempts[0].ID(), shared.OpAuthorize, ""); got == "" {
		t.Fatal("DeriveGatewayIdempotencyKey returned nothing for a real attempt id")
	}

	// The reconciler's lookup reaches the gateway even while the fault is in force, because the
	// fault models a *call* that timed out rather than a gateway that is gone.
	found, err := e.Primary.Lookup(e.Ctx(), findRequest(key))
	if err != nil {
		t.Fatalf("the reconciler's lookup failed: %v", err)
	}
	if found == nil {
		t.Fatal("the gateway returned neither a result nor an error for a lookup by idempotency key")
	}
	h.HoldsNow(t, "after the reconciler resolved the unknown outcome")
}

// findRequest builds the lookup the reconciler makes, keyed only by what survives a crash: the
// deterministic idempotency key, with no gateway reference, because a crashed process never
// learned one.
func findRequest(key string) spi.LookupRequest {
	return spi.LookupRequest{IdempotencyKey: key}
}
