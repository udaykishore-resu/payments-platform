package postgres_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/enginetest"
	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine/postgres"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// harness wires an engine against the in-memory repository. Every test in this package builds
// one; nothing here touches a database, a clock or a registry that outlives the test.
type harness struct {
	t       *testing.T
	clock   *enginetest.Clock
	repo    *enginetest.Repo
	acts    *engine.Activities
	metrics *enginetest.Metrics
	audit   *enginetest.Auditor
	runs    *enginetest.Recorder
	eng     *postgres.Engine

	mu  sync.Mutex
	ids int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		t:       t,
		clock:   enginetest.NewClock(time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)),
		acts:    engine.NewActivities(),
		metrics: enginetest.NewMetrics(),
		audit:   enginetest.NewAuditor(),
		runs:    enginetest.NewRecorder(),
	}
	h.repo = enginetest.NewRepo(h.clock)

	eng, err := postgres.New(postgres.Options{
		Repo:        h.repo,
		Activities:  h.acts,
		Definitions: engine.NewRegistry(),
		Clock:       h.clock,
		Metrics:     h.metrics,
		Auditor:     h.audit,
		WorkerID:    "worker-a/pid1/boot1",
		Salt:        []byte("test-salt"),
		Logger:      discardLogger(),
		NewID:       h.nextID,
		Tenant:      func(context.Context) shared.TenantID { return "ten_test" },
	})
	if err != nil {
		t.Fatalf("building the engine: %v", err)
	}
	h.eng = eng
	return h
}

// discardLogger silences the engine's structured logs in tests.
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func (h *harness) nextID() shared.WorkflowID {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ids++
	return shared.WorkflowID("wfr_test_" + strconv.Itoa(h.ids))
}

// scripted registers an activity whose behaviour a test controls per attempt.
func (h *harness) scripted(name string, fn func(ctx context.Context, in engine.Input, attempt int) (engine.Output, error)) {
	h.t.Helper()
	var mu sync.Mutex
	attempts := 0
	err := h.acts.Register(engine.ActivityFunc{
		ActivityName: name,
		Fn: func(ctx context.Context, in engine.Input) (engine.Output, error) {
			mu.Lock()
			attempts++
			n := attempts
			mu.Unlock()
			h.runs.Note(name)
			return fn(ctx, in, n)
		},
	})
	if err != nil {
		h.t.Fatalf("registering %s: %v", name, err)
	}
}

// ok registers an activity that always succeeds, echoing its step name.
func (h *harness) ok(name string) {
	h.t.Helper()
	h.scripted(name, func(_ context.Context, in engine.Input, _ int) (engine.Output, error) {
		out, _ := json.Marshal(map[string]string{"step": in.Step, "key": in.IdempotencyKey})
		return out, nil
	})
}

func (h *harness) instance(id shared.WorkflowID) *engine.Instance {
	h.t.Helper()
	inst, err := h.eng.Get(context.Background(), id)
	if err != nil {
		h.t.Fatalf("Get(%s): %v", id, err)
	}
	return inst
}

func (h *harness) resume(id shared.WorkflowID) {
	h.t.Helper()
	if err := h.eng.Resume(context.Background(), id); err != nil {
		h.t.Fatalf("Resume(%s): %v", id, err)
	}
}

// linear builds an n-step definition of always-succeeding activities.
func linearDefinition(t *testing.T, h *harness, names ...string) *engine.Definition {
	t.Helper()
	steps := make([]engine.Step, 0, len(names))
	for _, n := range names {
		h.ok(n)
		steps = append(steps, engine.Step{
			Name:       n,
			Activity:   n,
			Timeout:    5 * time.Second,
			Idempotent: true,
			Retry:      engine.RetryPolicy{MaxAttempts: 3, InitialInterval: 200 * time.Millisecond, MaxInterval: time.Second, BackoffFactor: 2},
		})
	}
	return &engine.Definition{Name: "linear", Version: 1, Steps: steps}
}

// transient is a retryable platform error.
func transient(msg string) error {
	return apierror.New(apierror.CodeServiceUnavailable, msg)
}

// business is a non-retryable business rejection.
func business(msg string) error {
	return apierror.New(apierror.CodeValidationFailed, msg)
}

func mustStart(t *testing.T, h *harness, def *engine.Definition, key string, input any) shared.WorkflowID {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshalling input: %v", err)
	}
	id, err := h.eng.Start(context.Background(), def, key, raw)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return id
}
