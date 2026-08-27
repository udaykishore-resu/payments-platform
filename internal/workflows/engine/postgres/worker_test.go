package postgres_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
)

func TestWorkerDrivesInstancesToCompletion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a", "b", "c")

	for _, key := range []string{"mrc_1", "mrc_2", "mrc_3"} {
		mustStart(t, h, def, key, nil)
	}

	worker := postgres.NewWorker(h.eng, def.Name)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := worker.DriveOnce(ctx)
		if err != nil {
			t.Fatalf("DriveOnce: %v", err)
		}
		if n == 0 {
			break
		}
	}

	for _, key := range []string{"mrc_1", "mrc_2", "mrc_3"} {
		rec, err := h.repo.GetInstanceByBusinessKey(ctx, def.Name, key)
		if err == nil && rec != nil {
			t.Fatalf("%s is still live in state %s", key, rec.State)
		}
	}
	if got := h.runs.Runs("c"); got != 3 {
		t.Fatalf("step c ran %d times across 3 instances, want 3", got)
	}
}

// TestWorkerStopsCleanlyAndLeaksNoGoroutines is the shutdown contract: Run returns only after
// every goroutine it owns has exited, and the in-flight step is allowed to finish rather than
// being abandoned mid-call. Abandoning an external call is what produces the ambiguity the whole
// engine is built to avoid.
func TestWorkerStopsCleanlyAndLeaksNoGoroutines(t *testing.T) {
	h := newHarness(t)

	var mu sync.Mutex
	finished := false
	h.scripted("slow", func(ctx context.Context, _ engine.Input, _ int) (engine.Output, error) {
		// The activity ignores cancellation on purpose: it stands in for an eight-second gateway
		// call that must not be abandoned. The worker must wait for it.
		time.Sleep(80 * time.Millisecond)
		mu.Lock()
		finished = true
		mu.Unlock()
		return engine.Output(`{"ok":true}`), nil
	})
	def := &engine.Definition{Name: "slow-shutdown", Version: 1, Steps: []engine.Step{
		{Name: "slow", Activity: "slow", Timeout: 5 * time.Second, Idempotent: true,
			Retry: engine.RetryPolicy{MaxAttempts: 1}},
	}}
	id := mustStart(t, h, def, "mrc_1", nil)

	before := runtime.NumGoroutine()

	eng, err := postgres.New(postgres.Options{
		Repo: h.repo, Activities: h.acts, Definitions: h.eng.Definitions(),
		Clock: h.clock, Metrics: h.metrics, Auditor: h.audit,
		WorkerID: "worker-loop", Salt: []byte("t"),
		PollInterval: 5 * time.Millisecond, ReapInterval: 10 * time.Millisecond,
		StuckInterval: 10 * time.Millisecond, Logger: discardLogger(),
	})
	if err != nil {
		t.Fatalf("building the worker engine: %v", err)
	}
	worker := postgres.NewWorker(eng, def.Name)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	// Let the poller claim and start the step, then ask it to stop mid-step.
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	mu.Lock()
	ran := finished
	mu.Unlock()
	if !ran {
		t.Fatal("the in-flight step was abandoned mid-call rather than finished")
	}
	if got := h.instance(id).State; got != engine.InstanceCompleted {
		t.Fatalf("the finished step was not checkpointed; state = %s", got)
	}

	// Goroutines exit asynchronously; give the runtime a moment before comparing.
	after := before
	for i := 0; i < 50; i++ {
		after = runtime.NumGoroutine()
		if after <= before+2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > before+2 {
		t.Fatalf("goroutines leaked: %d before, %d after", before, after)
	}
}

func TestStuckDetectionReportsAnInstanceMakingNoProgress(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a")
	id := mustStart(t, h, def, "mrc_1", nil)

	// Nothing has run it, and its RunAfter is long past: it should be moving and is not.
	h.clock.Advance(2 * postgres.DefaultStuckThreshold)

	stuck, err := h.eng.DetectStuck(context.Background(), 10)
	if err != nil {
		t.Fatalf("DetectStuck: %v", err)
	}
	if len(stuck) != 1 || stuck[0].ID != id {
		t.Fatalf("DetectStuck returned %+v, want the pending instance", stuck)
	}
	if stuck[0].NoProgressFor < postgres.DefaultStuckThreshold {
		t.Fatalf("NoProgressFor = %s, want more than the threshold", stuck[0].NoProgressFor)
	}
}

func TestStuckDetectionIgnoresASignalWait(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := gatedDefinition(t, h)
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	// A signal wait is *supposed* to be long. Counting it as stuck would drown the real signal
	// in noise; it has its own timeout and its own metric.
	h.clock.Advance(4 * 24 * time.Hour)

	stuck, err := h.eng.DetectStuck(context.Background(), 10)
	if err != nil {
		t.Fatalf("DetectStuck: %v", err)
	}
	if len(stuck) != 0 {
		t.Fatalf("a WAITING_SIGNAL instance was reported stuck: %+v", stuck)
	}
}

func TestPublishInstanceCountsCoversEveryState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	def := linearDefinition(t, h, "a")
	id := mustStart(t, h, def, "mrc_1", nil)
	h.resume(id)

	if err := h.eng.PublishInstanceCounts(context.Background(), def.Name); err != nil {
		t.Fatalf("PublishInstanceCounts: %v", err)
	}
	if got := h.metrics.Instances(def.Name, string(engine.InstanceCompleted)); got != 1 {
		t.Fatalf("COMPLETED gauge = %v, want 1", got)
	}
	// Every state is published, including the empty ones: a gauge that stops being exported
	// when it reaches zero looks identical to a gauge whose exporter died.
	if got := h.metrics.Instances(def.Name, string(engine.InstanceFailed)); got != 0 {
		t.Fatalf("FAILED gauge = %v, want an explicit 0", got)
	}
}
