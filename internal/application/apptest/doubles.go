package apptest

import (
	"context"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/adapters/gateway/spi"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// The doubles in this file are declared *structurally*: none of them names an interface from a
// use-case package. That is not stylistic — an apptest that imported the payment package could
// not be imported by the payment package's own tests without an import cycle. Satisfying the
// interfaces by shape keeps one set of doubles usable from every use-case package and from the
// integration tests.

// Breaker is a scriptable circuit breaker.
//
// It records every outcome with its `counted` bit, which is what makes "a declined outcome does
// not count against the circuit breaker" an assertion about behaviour rather than about
// intentions: a decline that arrives with counted=true would open the breaker on a perfectly
// healthy gateway for every merchant sharing it.
type Breaker struct {
	mu sync.Mutex
	// Open lists the keys the breaker refuses. A test sets it to drive the circuit-open path.
	Open map[string]bool
	// Outcomes is the ordered log of (key, success, counted) the breaker was told about.
	Outcomes []BreakerOutcome
}

// BreakerOutcome is one recorded call outcome.
type BreakerOutcome struct {
	Key     string
	Success bool
	Counted bool
}

// NewBreaker returns a closed breaker.
func NewBreaker() *Breaker { return &Breaker{Open: map[string]bool{}} }

// Allow reports whether a call may proceed and returns the outcome recorder.
func (b *Breaker) Allow(key string) (func(success, counted bool), error) {
	b.mu.Lock()
	open := b.Open[key]
	b.mu.Unlock()
	if open {
		return nil, apierror.Newf(apierror.CodeGatewayCircuitOpen, "apptest: circuit open for %s", key)
	}
	return func(success, counted bool) {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.Outcomes = append(b.Outcomes, BreakerOutcome{Key: key, Success: success, Counted: counted})
	}, nil
}

// State returns 0 (closed) or 1 (open).
func (b *Breaker) State(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.Open[key] {
		return 1
	}
	return 0
}

// CountedOutcomes returns only the outcomes that were counted toward the error rate.
func (b *Breaker) CountedOutcomes() []BreakerOutcome {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []BreakerOutcome
	for _, o := range b.Outcomes {
		if o.Counted {
			out = append(out, o)
		}
	}
	return out
}

// Bulkhead is an unbounded bulkhead that records acquisitions and releases.
//
// It also verifies the thing that actually goes wrong with bulkheads in production: a release
// that never runs. InFlight is asserted to be zero at the end of a test, which catches a leaked
// slot that would otherwise only show up as a slow strangulation under load.
type Bulkhead struct {
	mu       sync.Mutex
	InFlight int
	Peak     int
	// Refuse makes Acquire fail for a key, to drive the saturation path.
	Refuse map[string]bool
}

// NewBulkhead returns an empty bulkhead.
func NewBulkhead() *Bulkhead { return &Bulkhead{Refuse: map[string]bool{}} }

// Acquire takes a slot.
func (b *Bulkhead) Acquire(ctx context.Context, key string) (func(), error) {
	b.mu.Lock()
	if b.Refuse[key] {
		b.mu.Unlock()
		return nil, apierror.Newf(apierror.CodeConcurrencyLimitExceeded, "apptest: bulkhead full for %s", key)
	}
	b.InFlight++
	if b.InFlight > b.Peak {
		b.Peak = b.InFlight
	}
	b.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			b.InFlight--
			b.mu.Unlock()
		})
	}, nil
}

// Metrics records every telemetry call so a test can assert on labels without a registry.
type Metrics struct {
	mu       sync.Mutex
	Outcomes []string
	Routing  []string
	Stages   map[string]time.Duration
	Gateway  []string
}

// NewMetrics returns an empty recorder.
func NewMetrics() *Metrics { return &Metrics{Stages: map[string]time.Duration{}} }

// RecordPaymentOutcome records a terminal payment outcome label.
func (m *Metrics) RecordPaymentOutcome(outcome string, _ money.Currency, _ shared.PaymentMethod, gw shared.GatewayID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Outcomes = append(m.Outcomes, outcome+"@"+gw.String())
}

// ObserveGatewayRequest records one gateway call.
func (m *Metrics) ObserveGatewayRequest(gw shared.GatewayID, op shared.Operation, outcome string, _ time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Gateway = append(m.Gateway, gw.String()+":"+string(op)+":"+outcome)
}

// RecordRoutingDecision records one selection.
func (m *Metrics) RecordRoutingDecision(gw shared.GatewayID, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Routing = append(m.Routing, gw.String()+":"+reason)
}

// SetCircuitState records a circuit-state gauge write.
func (m *Metrics) SetCircuitState(shared.GatewayID, shared.Operation, int) {}

// RecordIdempotencyOutcome records an idempotency outcome label.
func (m *Metrics) RecordIdempotencyOutcome(string) {}

// ObserveStage records a pipeline stage duration.
func (m *Metrics) ObserveStage(stage string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Stages[stage] = d
}

// PaymentOutcomes returns the recorded outcome labels.
func (m *Metrics) PaymentOutcomes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.Outcomes...)
}

// Auditor records audit calls into the store, inside whatever transaction it was handed.
//
// It writes through the Repositories bundle rather than to its own slice so that a rolled-back
// transaction loses its audit record too — which is the property the "audit and state change
// commit together" requirement actually means.
type Auditor struct{ Store *Store }

// Record appends an audit line.
func (a *Auditor) Record(ctx context.Context, r ports.Repositories, action, resourceType, resourceID, outcome string, detail map[string]any) error {
	a.Store.mu.Lock()
	defer a.Store.mu.Unlock()
	a.Store.AuditLines = append(a.Store.AuditLines, AuditLine{
		Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Outcome: outcome, Detail: detail,
	})
	return nil
}

// Actions returns the recorded action names in order.
func (a *Auditor) Actions() []string {
	a.Store.mu.Lock()
	defer a.Store.mu.Unlock()
	out := make([]string, 0, len(a.Store.AuditLines))
	for _, l := range a.Store.AuditLines {
		out = append(out, l.Action)
	}
	return out
}

// Secrets is an in-memory secrets provider.
type Secrets struct {
	mu     sync.Mutex
	values map[string]map[string]string
	// Err, when set, makes every Get fail, to drive the credential-unresolvable path.
	Err error
}

// NewSecrets returns an empty provider.
func NewSecrets() *Secrets { return &Secrets{values: map[string]map[string]string{}} }

// Seed stores material without a context, for test setup.
func (s *Secrets) Seed(ref string, material map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[ref] = material
}

// Put stores material and returns a versioned reference.
func (s *Secrets) Put(ctx context.Context, ref string, m map[string]string) (string, error) {
	s.Seed(ref, m)
	return ref + "#1", nil
}

// Get resolves a reference.
func (s *Secrets) Get(ctx context.Context, ref string) (ports.SecretMaterial, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Err != nil {
		return nil, s.Err
	}
	v, ok := s.values[ref]
	if !ok {
		return nil, apierror.Newf(apierror.CodeInternalError, "apptest: no secret at %s", ref)
	}
	return material{v: v}, nil
}

// Rotate writes a new version.
func (s *Secrets) Rotate(ctx context.Context, ref string, m map[string]string, overlap time.Duration) (string, error) {
	s.Seed(ref, m)
	return ref + "#2", nil
}

// Delete removes a reference.
func (s *Secrets) Delete(ctx context.Context, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.values, ref)
	return nil
}

type material struct{ v map[string]string }

func (m material) Value(field string) (string, bool) { v, ok := m.v[field]; return v, ok }

func (m material) Fields() []string {
	out := make([]string, 0, len(m.v))
	for k := range m.v {
		out = append(out, k)
	}
	return out
}

func (m material) Version() string { return "1" }

// Velocity is an in-memory windowed counter store.
type Velocity struct {
	mu      sync.Mutex
	counts  map[string]int64
	volumes map[string]money.Money
	// FailKeys makes reads of the listed keys fail, which is how a test drives the
	// "an unavailable counter is never a zero" path.
	FailKeys map[string]bool
}

// NewVelocity returns an empty counter store.
func NewVelocity() *Velocity {
	return &Velocity{counts: map[string]int64{}, volumes: map[string]money.Money{}, FailKeys: map[string]bool{}}
}

// Set seeds a counter.
func (v *Velocity) Set(key string, n int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.counts[key] = n
}

// IncrementAndCount bumps and reads atomically.
func (v *Velocity) IncrementAndCount(ctx context.Context, key string, window time.Duration) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.FailKeys[key] {
		return 0, apierror.New(apierror.CodeDependencyFailure, "apptest: counter store unavailable")
	}
	v.counts[key]++
	return v.counts[key], nil
}

// Count reads a windowed count.
func (v *Velocity) Count(ctx context.Context, key string, window time.Duration) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.FailKeys[key] {
		return 0, apierror.New(apierror.CodeDependencyFailure, "apptest: counter store unavailable")
	}
	return v.counts[key], nil
}

// SumAndAdd reads and adds a money-valued counter.
func (v *Velocity) SumAndAdd(ctx context.Context, key string, window time.Duration, add money.Money) (money.Money, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.FailKeys[key] {
		return money.Money{}, apierror.New(apierror.CodeDependencyFailure, "apptest: counter store unavailable")
	}
	cur, ok := v.volumes[key]
	if !ok {
		cur = money.Zero(add.Currency())
	}
	next, err := cur.Add(add)
	if err != nil {
		return money.Money{}, err
	}
	v.volumes[key] = next
	return next, nil
}

// GatewayCall is one recorded adapter invocation.
type GatewayCall struct {
	Op             shared.Operation
	IdempotencyKey string
	Amount         money.Money
}

// GatewayScript is one scripted response.
//
// A script rather than a stub with a switch: the tests that matter are about *sequences* — a
// timeout then nothing, a soft decline then a success at a different gateway — and a script makes
// the sequence the thing the test declares.
type GatewayScript struct {
	Result *spi.Result
	Err    error
	// Delay, when set, sleeps before answering. Used only by the timeout tests, and kept short.
	Delay time.Duration
}

// Gateway is a scripted adapter that records the order of its calls into a Recorder.
//
// The Recorder is the load-bearing part. The single most important invariant in the platform is
// that the attempt row is committed *before* the gateway is called, and both orderings succeed:
// nothing but the sequence distinguishes correct from catastrophic. Interleaving the repository's
// writes and this adapter's calls into one ordered log is the only way to assert it.
type Gateway struct {
	id  shared.GatewayID
	rec *Recorder

	mu      sync.Mutex
	scripts map[shared.Operation][]GatewayScript
	calls   []GatewayCall
}

// NewGateway returns a scripted adapter.
func NewGateway(id shared.GatewayID, rec *Recorder) *Gateway {
	if rec == nil {
		rec = NewRecorder()
	}
	return &Gateway{id: id, rec: rec, scripts: map[shared.Operation][]GatewayScript{}}
}

// Script appends a scripted answer for an operation. Answers are consumed in order; when the
// script runs out the last answer repeats, so a test only declares the steps it cares about.
func (g *Gateway) Script(op shared.Operation, s GatewayScript) *Gateway {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scripts[op] = append(g.scripts[op], s)
	return g
}

// ID returns the gateway slug.
func (g *Gateway) ID() shared.GatewayID { return g.id }

// Calls returns the recorded invocations.
func (g *Gateway) Calls() []GatewayCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]GatewayCall(nil), g.calls...)
}

func (g *Gateway) next(op shared.Operation, key string, amount money.Money) (*spi.Result, error) {
	g.rec.Record("gateway.call:" + g.id.String() + ":" + string(op))
	g.rec.Record("gateway.call")
	g.mu.Lock()
	g.calls = append(g.calls, GatewayCall{Op: op, IdempotencyKey: key, Amount: amount})
	scripts := g.scripts[op]
	var s GatewayScript
	switch {
	case len(scripts) == 0:
		g.mu.Unlock()
		return nil, spi.ErrNotSupported
	case len(scripts) == 1:
		s = scripts[0]
	default:
		s = scripts[0]
		g.scripts[op] = scripts[1:]
	}
	g.mu.Unlock()
	if s.Delay > 0 {
		time.Sleep(s.Delay)
	}
	return s.Result, s.Err
}

// Authorize places a hold.
func (g *Gateway) Authorize(ctx context.Context, req spi.AuthorizeRequest) (*spi.Result, error) {
	return g.next(shared.OpAuthorize, req.IdempotencyKey, req.Amount)
}

// Capture converts a hold into a debit.
func (g *Gateway) Capture(ctx context.Context, req spi.CaptureRequest) (*spi.Result, error) {
	return g.next(shared.OpCapture, req.IdempotencyKey, req.Amount)
}

// Refund returns captured funds.
func (g *Gateway) Refund(ctx context.Context, req spi.RefundRequest) (*spi.Result, error) {
	return g.next(shared.OpRefund, req.IdempotencyKey, req.Amount)
}

// Void releases an authorization.
func (g *Gateway) Void(ctx context.Context, req spi.VoidRequest) (*spi.Result, error) {
	return g.next(shared.OpVoid, req.IdempotencyKey, money.Money{})
}

// Lookup asks what happened to a transaction.
func (g *Gateway) Lookup(ctx context.Context, req spi.LookupRequest) (*spi.Result, error) {
	return g.next(shared.OpLookup, req.IdempotencyKey, money.Money{})
}

// Scorer is a scriptable external risk model.
type Scorer struct {
	// Value is the score a successful call returns.
	Value int
	// Err makes the call fail, so a test can assert that unavailability degrades to the policy
	// posture rather than to an approval.
	Err error
	// Delay makes the call exceed its deadline.
	Delay time.Duration
}

// Score returns the scripted result.
func (s *Scorer) Score(ctx context.Context, req ports.RiskScoreRequest) (ports.RiskScoreResult, error) {
	if s.Delay > 0 {
		select {
		case <-time.After(s.Delay):
		case <-ctx.Done():
			return ports.RiskScoreResult{}, ctx.Err()
		}
	}
	if s.Err != nil {
		return ports.RiskScoreResult{}, s.Err
	}
	return ports.RiskScoreResult{Score: s.Value}, nil
}

// Principal is a static authenticated caller.
type Principal struct {
	ID     string
	Scopes []string
	// Absent makes FromContext report no principal, which every scope rule must treat as a
	// refusal rather than as an unrestricted caller.
	Absent bool
}

// FromContext returns the principal.
func (p *Principal) FromContext(ctx context.Context) (string, []string, bool) {
	if p == nil || p.Absent {
		return "", nil, false
	}
	return p.ID, append([]string(nil), p.Scopes...), true
}

// AllScopes is the scope set a fully-authorized operator holds. Tests that are not about
// authorization use it so that a missing scope never masks the behaviour under test.
func AllScopes() []string {
	return []string{
		"payments:write", "payments:capture", "payments:refund", "payments:void",
		"payments:refund:elevated", "merchants:write", "onboarding:approve", "config:publish",
	}
}
