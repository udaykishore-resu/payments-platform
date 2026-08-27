package enginetest

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/workflows/engine"
)

// StepObservation is one recorded call to Metrics.ObserveStepDuration.
type StepObservation struct {
	Workflow string
	Step     string
	Outcome  string
	Duration time.Duration
}

// Metrics records the two engine metric series so a test can assert that a step was observed
// with the outcome it actually had. A metric hook that is never asserted on is a metric hook
// that silently stops firing.
type Metrics struct {
	mu        sync.Mutex
	steps     []StepObservation
	instances map[string]float64
}

var _ engine.Metrics = (*Metrics)(nil)

// NewMetrics returns an empty recorder.
func NewMetrics() *Metrics { return &Metrics{instances: make(map[string]float64, 8)} }

// ObserveStepDuration implements engine.Metrics.
func (m *Metrics) ObserveStepDuration(_ context.Context, workflow, step, outcome string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.steps = append(m.steps, StepObservation{Workflow: workflow, Step: step, Outcome: outcome, Duration: d})
}

// SetInstances implements engine.Metrics.
func (m *Metrics) SetInstances(_ context.Context, workflow, state string, n float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instances[workflow+"/"+state] = n
}

// Steps returns the recorded step observations.
func (m *Metrics) Steps() []StepObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]StepObservation(nil), m.steps...)
}

// Outcome returns the last recorded outcome for a step, or "".
func (m *Metrics) Outcome(step string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := ""
	for _, s := range m.steps {
		if s.Step == step {
			out = s.Outcome
		}
	}
	return out
}

// Instances returns the last published gauge value for a (workflow, state) pair.
func (m *Metrics) Instances(workflow, state string) float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.instances[workflow+"/"+state]
}

// Auditor records engine audit events so a test can assert that a manual gate's approval was
// attributed to the principal who sent it.
type Auditor struct {
	mu     sync.Mutex
	events []engine.AuditEvent
}

var _ engine.Auditor = (*Auditor)(nil)

// NewAuditor returns an empty recorder.
func NewAuditor() *Auditor { return &Auditor{} }

// Record implements engine.Auditor.
func (a *Auditor) Record(_ context.Context, e engine.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

// Events returns the recorded audit events.
func (a *Auditor) Events() []engine.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]engine.AuditEvent(nil), a.events...)
}

// Find returns the first recorded event with the given action, or nil.
func (a *Auditor) Find(action string) *engine.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.events {
		if a.events[i].Action == action {
			e := a.events[i]
			return &e
		}
	}
	return nil
}

// Recorder counts activity executions per step, which is how the crash-and-resume tests assert
// the property that matters: a completed step is never executed a second time.
type Recorder struct {
	mu    sync.Mutex
	runs  map[string]int
	order []string
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{runs: make(map[string]int, 16)} }

// Note records one execution of name.
func (r *Recorder) Note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[name]++
	r.order = append(r.order, name)
}

// Runs returns how many times name executed.
func (r *Recorder) Runs(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runs[name]
}

// Order returns the execution order, which is what the compensation test asserts on: reverse
// order is a sequence property, and a set of counts cannot express it.
func (r *Recorder) Order() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// Reset clears the recorder between phases of a test.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = make(map[string]int, 16)
	r.order = nil
}
