// Package apptest holds in-memory implementations of every port the application layer declares.
//
// It is a normal (non `_test.go`) package on purpose. The same doubles are used by the use-case
// unit tests in `internal/application/**` and by the integration tests that run the same use
// cases against real infrastructure with one dependency swapped at a time; a `_test.go` helper
// cannot be imported across packages, and duplicating it produces two fakes that drift until a
// test passes against one and fails against the other.
//
// Three rules the doubles follow, because a fake that is more permissive than the real thing is
// a test that proves nothing:
//
//  1. **The unit of work is real.** Within runs the callback and, if it returns an error, rolls
//     back every write the callback made. A fake that committed regardless would make every
//     "the state change and the event commit together" assertion vacuous.
//  2. **Optimistic concurrency is real.** Save refuses a stale version, exactly as the Postgres
//     repository does, so a test can exercise the conflict path.
//  3. **Uniqueness is enforced where the database enforces it.** One successful attempt per
//     payment, one webhook per (gateway, event id), one live workflow per business key.
//
// A note on documentation. The repository doubles below implement the interfaces in
// internal/application/ports method for method, and the contract of each method — what it
// promises, what it refuses, and why — is documented once, on the port. Repeating it here would
// produce two statements of one contract, which is how the double and the real implementation
// start to disagree. Each double's *type* carries the note that matters: what it enforces that a
// naive fake would not. Where a method's behaviour deviates from, or deliberately reproduces, a
// specific guarantee, it carries its own comment.
package apptest

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/audit"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/internal/domain/tenant"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

// Clock is a deterministic clock that advances only when a test says so.
//
// Deterministic rather than monotonic-with-sleeps because every time-dependent rule in this
// platform — authorization expiry, refund windows, idempotency leases, signature freshness — has
// a test, and a test that waits 180 days to check the refund window is a test nobody runs.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock returns a clock fixed at t.
func NewClock(t time.Time) *Clock { return &Clock{t: t.UTC()} }

// Now returns the current instant.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward and returns the new instant.
func (c *Clock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// Store is the in-memory database behind every repository double.
//
// One struct rather than one per repository because the repositories share a transaction, and a
// rollback has to undo writes across all of them. Splitting the state would make the unit of
// work a lie in exactly the case the tests exist to check.
type Store struct {
	mu sync.Mutex

	Payments    map[shared.PaymentID]*paymentRow
	Merchants   map[shared.MerchantID]*merchantRow
	Tenants     map[shared.TenantID]*tenant.Tenant
	Gateways    map[shared.GatewayID]*gateway.Gateway
	Connections map[shared.ConnectionID]*gateway.Connection
	Health      map[string]*gateway.Health
	Configs     map[shared.MerchantID][]*config.MerchantConfig
	Idempotency map[string]ports.IdempotencyRecord
	Outbox      []ports.OutboxMessage
	Ledger      []*ledger.Transaction
	Audit       []audit.Record
	AuditLines  []AuditLine
	Webhooks    map[shared.WebhookID]*ports.InboundWebhook
	WebhookSeen map[string]shared.WebhookID
	Workflows   map[shared.WorkflowID]*ports.WorkflowInstanceRecord
	WorkflowKey map[string]shared.WorkflowID
	Steps       map[shared.WorkflowID][]ports.WorkflowStepRecord
	Exceptions  []ports.ReconciliationException
	Secrets     map[string]map[string]string
	// Claims is the deduplication log a consumer claims against before writing its effect.
	Claims map[string]struct{}

	// FailNextSave makes the next payment save return a retryable conflict, so a test can drive
	// the path a version mismatch takes without racing two goroutines to produce one.
	FailNextSave error
}

// paymentRow holds a payment and the version it was persisted at, so the double can enforce the
// same optimistic-concurrency contract the real repository does.
type paymentRow struct {
	p       *payment.Payment
	version shared.Version
}

type merchantRow struct {
	m       *merchant.Merchant
	version shared.Version
}

// AuditLine is one recorded audit call, in the shape the application layer writes them.
type AuditLine struct {
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      string
	Detail       map[string]any
}

// NewStore returns an empty store.
func NewStore() *Store {
	return &Store{
		Payments:    map[shared.PaymentID]*paymentRow{},
		Merchants:   map[shared.MerchantID]*merchantRow{},
		Tenants:     map[shared.TenantID]*tenant.Tenant{},
		Gateways:    map[shared.GatewayID]*gateway.Gateway{},
		Connections: map[shared.ConnectionID]*gateway.Connection{},
		Health:      map[string]*gateway.Health{},
		Configs:     map[shared.MerchantID][]*config.MerchantConfig{},
		Idempotency: map[string]ports.IdempotencyRecord{},
		Webhooks:    map[shared.WebhookID]*ports.InboundWebhook{},
		WebhookSeen: map[string]shared.WebhookID{},
		Workflows:   map[shared.WorkflowID]*ports.WorkflowInstanceRecord{},
		WorkflowKey: map[string]shared.WorkflowID{},
		Steps:       map[shared.WorkflowID][]ports.WorkflowStepRecord{},
		Secrets:     map[string]map[string]string{},
		Claims:      map[string]struct{}{},
	}
}

// snapshot copies the parts of the store a rollback must restore.
func (s *Store) snapshot() *Store {
	c := &Store{
		Payments:    make(map[shared.PaymentID]*paymentRow, len(s.Payments)),
		Merchants:   make(map[shared.MerchantID]*merchantRow, len(s.Merchants)),
		Tenants:     s.Tenants,
		Gateways:    s.Gateways,
		Connections: make(map[shared.ConnectionID]*gateway.Connection, len(s.Connections)),
		Health:      s.Health,
		Configs:     make(map[shared.MerchantID][]*config.MerchantConfig, len(s.Configs)),
		Idempotency: make(map[string]ports.IdempotencyRecord, len(s.Idempotency)),
		Outbox:      append([]ports.OutboxMessage(nil), s.Outbox...),
		Ledger:      append([]*ledger.Transaction(nil), s.Ledger...),
		Audit:       append([]audit.Record(nil), s.Audit...),
		AuditLines:  append([]AuditLine(nil), s.AuditLines...),
		Webhooks:    make(map[shared.WebhookID]*ports.InboundWebhook, len(s.Webhooks)),
		WebhookSeen: make(map[string]shared.WebhookID, len(s.WebhookSeen)),
		Workflows:   make(map[shared.WorkflowID]*ports.WorkflowInstanceRecord, len(s.Workflows)),
		WorkflowKey: make(map[string]shared.WorkflowID, len(s.WorkflowKey)),
		Steps:       make(map[shared.WorkflowID][]ports.WorkflowStepRecord, len(s.Steps)),
		Exceptions:  append([]ports.ReconciliationException(nil), s.Exceptions...),
		Secrets:     s.Secrets,
		Claims:      make(map[string]struct{}, len(s.Claims)),
	}
	for k := range s.Claims {
		c.Claims[k] = struct{}{}
	}
	for k, v := range s.Payments {
		cp := *v
		c.Payments[k] = &cp
	}
	for k, v := range s.Merchants {
		cp := *v
		c.Merchants[k] = &cp
	}
	for k, v := range s.Connections {
		c.Connections[k] = v
	}
	for k, v := range s.Configs {
		c.Configs[k] = append([]*config.MerchantConfig(nil), v...)
	}
	for k, v := range s.Idempotency {
		c.Idempotency[k] = v
	}
	for k, v := range s.Webhooks {
		cp := *v
		c.Webhooks[k] = &cp
	}
	for k, v := range s.WebhookSeen {
		c.WebhookSeen[k] = v
	}
	for k, v := range s.Workflows {
		cp := *v
		c.Workflows[k] = &cp
	}
	for k, v := range s.WorkflowKey {
		c.WorkflowKey[k] = v
	}
	for k, v := range s.Steps {
		c.Steps[k] = append([]ports.WorkflowStepRecord(nil), v...)
	}
	return c
}

func (s *Store) restore(c *Store) {
	s.Payments, s.Merchants, s.Connections = c.Payments, c.Merchants, c.Connections
	s.Configs, s.Idempotency, s.Outbox = c.Configs, c.Idempotency, c.Outbox
	s.Ledger, s.Audit, s.AuditLines = c.Ledger, c.Audit, c.AuditLines
	s.Webhooks, s.WebhookSeen = c.Webhooks, c.WebhookSeen
	s.Workflows, s.WorkflowKey, s.Steps = c.Workflows, c.WorkflowKey, c.Steps
	s.Exceptions = c.Exceptions
	s.Claims = c.Claims
}

// UnitOfWork is a real transactional double: it snapshots, runs, and rolls back on error.
type UnitOfWork struct {
	store *Store
	mu    sync.Mutex
	// Ops records the order of operations across the whole test, which is what the
	// "attempt committed before the gateway call" assertion reads.
	Ops *Recorder
}

// inTxKey marks a context as already inside a transaction.
//
// The nesting guard is per *context*, not per unit-of-work instance, and that distinction
// matters: two concurrent requests legitimately share one UnitOfWork and must not see each
// other's transactions as nesting. A counter on the struct would report a false conflict under
// exactly the concurrency the idempotency tests exist to exercise.
type inTxKey struct{}

// NewUnitOfWork returns a unit of work over the store.
func NewUnitOfWork(s *Store, rec *Recorder) *UnitOfWork {
	if rec == nil {
		rec = NewRecorder()
	}
	return &UnitOfWork{store: s, Ops: rec}
}

// Within runs fn atomically.
func (u *UnitOfWork) Within(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	if _, nested := ctx.Value(inTxKey{}).(bool); nested {
		return apierror.New(apierror.CodeInternalError, "apptest: nested unit of work")
	}
	// The whole transaction is serialized. That is stricter than Postgres, and deliberately so:
	// a double whose rollback could interleave with another transaction's writes would restore a
	// snapshot containing them, which is a bug in the test harness reported as a bug in the code.
	u.mu.Lock()
	defer u.mu.Unlock()

	before := u.store.snapshot()
	err := fn(context.WithValue(ctx, inTxKey{}, true), u.store.repositories(u.Ops))
	if err != nil {
		u.store.restore(before)
	}
	return err
}

// WithinSerializable has the same semantics here: the double is already serialized by its mutex.
func (u *UnitOfWork) WithinSerializable(ctx context.Context, fn func(context.Context, ports.Repositories) error) error {
	return u.Within(ctx, fn)
}

// Recorder captures an ordered log of the operations a test cares about.
//
// Ordering is the only way to assert the single most important invariant in the platform — the
// attempt row is committed *before* the gateway is called — because both operations succeed in
// either order and nothing but the sequence distinguishes correct from catastrophic.
type Recorder struct {
	mu  sync.Mutex
	ops []string
}

// NewRecorder returns an empty recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Record appends an operation.
func (r *Recorder) Record(op string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, op)
}

// Ops returns a copy of the log.
func (r *Recorder) Ops() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.ops...)
}

// IndexOf returns the position of the first occurrence of op, or -1.
func (r *Recorder) IndexOf(op string) int {
	for i, x := range r.Ops() {
		if x == op {
			return i
		}
	}
	return -1
}

// Count returns how many times op was recorded.
func (r *Recorder) Count(op string) int {
	n := 0
	for _, x := range r.Ops() {
		if x == op {
			n++
		}
	}
	return n
}

func (s *Store) repositories(rec *Recorder) ports.Repositories {
	return ports.Repositories{
		Payments:       &PaymentRepo{s: s, rec: rec},
		Merchants:      &MerchantRepo{s: s},
		Tenants:        &TenantRepo{s: s},
		Gateways:       &GatewayRepo{s: s},
		Connections:    &ConnectionRepo{s: s},
		Health:         &HealthRepo{s: s},
		Configs:        &ConfigRepo{s: s},
		Idempotency:    &IdempotencyStore{s: s},
		Outbox:         &OutboxWriter{s: s, rec: rec},
		Ledger:         &LedgerRepo{s: s},
		Audit:          &AuditRepo{s: s},
		Webhooks:       &WebhookRepo{s: s},
		Workflows:      &WorkflowRepo{s: s},
		Reconciliation: &ReconciliationRepo{s: s},
	}
}

// --- payments ---------------------------------------------------------------------------------

// PaymentRepo is the in-memory payment repository.
type PaymentRepo struct {
	s   *Store
	rec *Recorder
}

// Create inserts a payment and drains its events into the outbox, as the real repository does.
func (r *PaymentRepo) Create(ctx context.Context, p *payment.Payment) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, ok := r.s.Payments[p.ID()]; ok {
		return apierror.New(apierror.CodePaymentAlreadyProcessed, "apptest: payment already exists")
	}
	r.s.Payments[p.ID()] = &paymentRow{p: p, version: p.Version()}
	r.drain(p)
	r.rec.Record("payment.create")
	return nil
}

func (r *PaymentRepo) Get(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	row, ok := r.s.Payments[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodePaymentNotFound, "apptest: payment %s not found", id)
	}
	return row.p, nil
}

func (r *PaymentRepo) GetForUpdate(ctx context.Context, id shared.PaymentID) (*payment.Payment, error) {
	return r.Get(ctx, id)
}

// Save persists a modified aggregate under optimistic concurrency.
func (r *PaymentRepo) Save(ctx context.Context, p *payment.Payment) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if err := r.s.FailNextSave; err != nil {
		r.s.FailNextSave = nil
		return err
	}
	row, ok := r.s.Payments[p.ID()]
	if !ok {
		return apierror.Newf(apierror.CodePaymentNotFound, "apptest: payment %s not found", p.ID())
	}
	if p.Version() < row.version {
		return apierror.New(apierror.CodeInternalError, "apptest: optimistic concurrency conflict")
	}
	// Invariant I3, which the database enforces with a partial unique index. Enforcing it here
	// too is what makes "failover never produces two successful attempts" a real assertion rather
	// than a statement about the orchestrator's intentions.
	success := 0
	for _, a := range p.Attempts() {
		if a.Outcome() == payment.OutcomeSuccess && a.Operation() == shared.OpAuthorize {
			success++
		}
	}
	if success > 1 {
		return apierror.New(apierror.CodeInternalError,
			"apptest: two successful authorization attempts on one payment (invariant I3)")
	}
	row.p, row.version = p, p.Version()
	r.drain(p)
	r.rec.Record("payment.save")
	return nil
}

// SaveAttempt persists one attempt without rewriting the aggregate. The recorder entry is what
// the ordering assertion keys on.
func (r *PaymentRepo) SaveAttempt(ctx context.Context, a *payment.Attempt) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.rec.Record("attempt.save:" + a.ID().String())
	r.rec.Record("attempt.save")
	return nil
}

func (r *PaymentRepo) List(ctx context.Context, f ports.PaymentFilter, page ports.Page) ([]*payment.Payment, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]*payment.Payment, 0, len(r.s.Payments))
	for _, row := range r.s.Payments {
		if !f.MerchantID.IsZero() && row.p.MerchantID() != f.MerchantID {
			continue
		}
		if len(f.States) > 0 && !containsState(f.States, row.p.State()) {
			continue
		}
		out = append(out, row.p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, "", nil
}

// FindUnresolved returns payments with an attempt awaiting reconciliation.
func (r *PaymentRepo) FindUnresolved(ctx context.Context, olderThan time.Duration, limit int) ([]*payment.Payment, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []*payment.Payment
	for _, row := range r.s.Payments {
		if row.p.HasUnresolvedAttempt() {
			out = append(out, row.p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *PaymentRepo) FindExpiredAuthorizations(ctx context.Context, now time.Time, limit int) ([]*payment.Payment, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []*payment.Payment
	for _, row := range r.s.Payments {
		exp := row.p.AuthExpiresAt()
		if row.p.State() == payment.StateAuthorized && exp != nil && !now.Before(*exp) {
			out = append(out, row.p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

func (r *PaymentRepo) CountOpen(ctx context.Context, m shared.MerchantID) (int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	n := 0
	for _, row := range r.s.Payments {
		if row.p.MerchantID() == m && !row.p.State().IsTerminal() {
			n++
		}
	}
	return n, nil
}

// drain moves the aggregate's pending events into the outbox inside the same transaction, which
// is the mechanism the whole outbox pattern rests on.
func (r *PaymentRepo) drain(p *payment.Payment) {
	for _, e := range p.DrainEvents() {
		r.s.Outbox = append(r.s.Outbox, ports.OutboxMessage{
			ID:            shared.NewEventID(),
			TenantID:      e.TenantID,
			Topic:         e.Type.Topic(),
			Type:          string(e.Type),
			AggregateID:   e.AggregateID(),
			AggregateType: "payment",
			PartitionKey:  e.AggregateID(),
			OccurredAt:    e.OccurredAt,
		})
	}
}

func containsState(set []payment.State, v payment.State) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// --- merchants, tenants, gateways ---------------------------------------------------------------

// MerchantRepo is the in-memory merchant repository.
type MerchantRepo struct{ s *Store }

func (r *MerchantRepo) Create(ctx context.Context, m *merchant.Merchant) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if _, ok := r.s.Merchants[m.ID()]; ok {
		return apierror.New(apierror.CodeInternalError, "apptest: merchant exists")
	}
	r.s.Merchants[m.ID()] = &merchantRow{m: m, version: m.Version()}
	m.DrainEvents()
	return nil
}

func (r *MerchantRepo) Get(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	row, ok := r.s.Merchants[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeMerchantNotFound, "apptest: merchant %s not found", id)
	}
	return row.m, nil
}

func (r *MerchantRepo) GetForUpdate(ctx context.Context, id shared.MerchantID) (*merchant.Merchant, error) {
	return r.Get(ctx, id)
}

func (r *MerchantRepo) GetByExternalRef(ctx context.Context, ref string) (*merchant.Merchant, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, row := range r.s.Merchants {
		if row.m.ExternalRef() == ref {
			return row.m, nil
		}
	}
	return nil, apierror.Newf(apierror.CodeMerchantNotFound, "apptest: merchant %q not found", ref)
}

func (r *MerchantRepo) Save(ctx context.Context, m *merchant.Merchant) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	row, ok := r.s.Merchants[m.ID()]
	if !ok {
		return apierror.Newf(apierror.CodeMerchantNotFound, "apptest: merchant %s not found", m.ID())
	}
	if m.Version() < row.version {
		return apierror.New(apierror.CodeInternalError, "apptest: optimistic concurrency conflict")
	}
	row.m, row.version = m, m.Version()
	m.DrainEvents()
	return nil
}

func (r *MerchantRepo) List(ctx context.Context, f ports.MerchantFilter, page ports.Page) ([]*merchant.Merchant, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]*merchant.Merchant, 0, len(r.s.Merchants))
	for _, row := range r.s.Merchants {
		if len(f.Statuses) > 0 && !containsStatus(f.Statuses, row.m.Status()) {
			continue
		}
		out = append(out, row.m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, "", nil
}

func (r *MerchantRepo) FindKYCExpiring(ctx context.Context, within time.Duration, limit int) ([]*merchant.Merchant, error) {
	return nil, nil
}

func containsStatus(set []merchant.Status, v merchant.Status) bool {
	for _, x := range set {
		if x == v {
			return true
		}
	}
	return false
}

// TenantRepo is the in-memory tenant repository.
type TenantRepo struct{ s *Store }

func (r *TenantRepo) Get(ctx context.Context, id shared.TenantID) (*tenant.Tenant, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	t, ok := r.s.Tenants[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeInternalError, "apptest: tenant %s not found", id)
	}
	return t, nil
}

func (r *TenantRepo) Save(ctx context.Context, t *tenant.Tenant) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Tenants[t.ID()] = t
	t.DrainEvents()
	return nil
}

func (r *TenantRepo) GetAPIClient(ctx context.Context, id shared.APIClientID) (*tenant.APIClient, error) {
	return nil, apierror.New(apierror.CodeInternalError, "apptest: no api clients")
}

// GatewayRepo is the in-memory gateway registry.
type GatewayRepo struct{ s *Store }

func (r *GatewayRepo) Get(ctx context.Context, id shared.GatewayID) (*gateway.Gateway, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	g, ok := r.s.Gateways[id]
	if !ok {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured, "apptest: gateway %s not registered", id)
	}
	return g, nil
}

func (r *GatewayRepo) List(ctx context.Context) ([]*gateway.Gateway, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]*gateway.Gateway, 0, len(r.s.Gateways))
	for _, g := range r.s.Gateways {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}

func (r *GatewayRepo) Save(ctx context.Context, g *gateway.Gateway) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Gateways[g.ID()] = g
	return nil
}

// ConnectionRepo is the in-memory connection repository.
type ConnectionRepo struct{ s *Store }

func (r *ConnectionRepo) Get(ctx context.Context, id shared.ConnectionID) (*gateway.Connection, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	c, ok := r.s.Connections[id]
	if !ok {
		return nil, apierror.New(apierror.CodeGatewayNotConfigured, "apptest: connection not found")
	}
	return c, nil
}

func (r *ConnectionRepo) GetByMerchantGateway(ctx context.Context, m shared.MerchantID, g shared.GatewayID) (*gateway.Connection, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, c := range r.s.Connections {
		if c.MerchantID() == m && c.GatewayID() == g {
			return c, nil
		}
	}
	return nil, apierror.New(apierror.CodeGatewayNotConfigured, "apptest: connection not found")
}

func (r *ConnectionRepo) ListForMerchant(ctx context.Context, m shared.MerchantID) ([]*gateway.Connection, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []*gateway.Connection
	for _, c := range r.s.Connections {
		if c.MerchantID() == m {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GatewayID() < out[j].GatewayID() })
	return out, nil
}

func (r *ConnectionRepo) Save(ctx context.Context, c *gateway.Connection) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Connections[c.ID()] = c
	c.DrainEvents()
	return nil
}

func (r *ConnectionRepo) FindCredentialsDueForRotation(ctx context.Context, olderThan time.Duration, limit int) ([]*gateway.Connection, error) {
	return nil, nil
}

// HealthRepo is the in-memory health repository.
type HealthRepo struct{ s *Store }

func healthKey(g shared.GatewayID, op shared.Operation) string { return g.String() + ":" + string(op) }

func (r *HealthRepo) Get(ctx context.Context, g shared.GatewayID, op shared.Operation) (*gateway.Health, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	h, ok := r.s.Health[healthKey(g, op)]
	if !ok {
		return nil, apierror.New(apierror.CodeInternalError, "apptest: no health record")
	}
	return h, nil
}

func (r *HealthRepo) ListAll(ctx context.Context) ([]*gateway.Health, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := make([]*gateway.Health, 0, len(r.s.Health))
	for _, h := range r.s.Health {
		out = append(out, h)
	}
	return out, nil
}

func (r *HealthRepo) Save(ctx context.Context, h *gateway.Health) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Health[healthKey(h.GatewayID(), h.Operation())] = h
	h.DrainEvents()
	return nil
}

// --- configuration -------------------------------------------------------------------------------

// ConfigRepo is the in-memory, append-only configuration repository.
type ConfigRepo struct{ s *Store }

func (r *ConfigRepo) GetActive(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	versions := r.s.Configs[m]
	for i := len(versions) - 1; i >= 0; i-- {
		if versions[i].Status == config.StatusActive {
			return versions[i], nil
		}
	}
	return nil, apierror.Newf(apierror.CodeConfigurationInvalid, "apptest: no active configuration for %s", m)
}

func (r *ConfigRepo) GetVersion(ctx context.Context, m shared.MerchantID, version int) (*config.MerchantConfig, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, c := range r.s.Configs[m] {
		if c.Version == version {
			return c, nil
		}
	}
	return nil, apierror.Newf(apierror.CodeConfigurationInvalid, "apptest: version %d not found", version)
}

func (r *ConfigRepo) ListVersions(ctx context.Context, m shared.MerchantID, page ports.Page) ([]*config.MerchantConfig, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := append([]*config.MerchantConfig(nil), r.s.Configs[m]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, "", nil
}

// Publish appends a version, enforcing the If-Match contract the real repository does.
func (r *ConfigRepo) Publish(ctx context.Context, c *config.MerchantConfig, expectedVersion int) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	current := 0
	for _, existing := range r.s.Configs[c.MerchantID] {
		if existing.Version > current {
			current = existing.Version
		}
	}
	if expectedVersion >= 0 && current != expectedVersion {
		return apierror.Newf(apierror.CodeConfigurationVersionConflict,
			"apptest: expected version %d, found %d", expectedVersion, current)
	}
	for _, existing := range r.s.Configs[c.MerchantID] {
		if existing.Status == config.StatusActive {
			existing.Status = config.StatusSuperseded
		}
	}
	r.s.Configs[c.MerchantID] = append(r.s.Configs[c.MerchantID], c)
	return nil
}

func (r *ConfigRepo) ListActiveSince(ctx context.Context, since time.Time, limit int) ([]*config.MerchantConfig, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []*config.MerchantConfig
	for _, versions := range r.s.Configs {
		for _, c := range versions {
			if c.Status == config.StatusActive && c.CreatedAt.After(since) {
				out = append(out, c)
			}
		}
	}
	return out, nil
}

// ConfigProvider is the fail-static snapshot double.
type ConfigProvider struct {
	mu       sync.Mutex
	snapshot map[shared.MerchantID]*config.MerchantConfig
	// Age is what SnapshotAge reports. A test sets it to drive the fail-static cliff.
	Age time.Duration
}

// NewConfigProvider returns an empty provider.
func NewConfigProvider() *ConfigProvider {
	return &ConfigProvider{snapshot: map[shared.MerchantID]*config.MerchantConfig{}}
}

// Put seeds the snapshot.
func (p *ConfigProvider) Put(c *config.MerchantConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.snapshot[c.MerchantID] = c
}

// SetAge sets the reported staleness.
func (p *ConfigProvider) SetAge(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.Age = d
}

// Get returns the merchant's snapshot.
func (p *ConfigProvider) Get(ctx context.Context, m shared.MerchantID) (*config.MerchantConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.snapshot[m]
	if !ok {
		return nil, apierror.New(apierror.CodeConfigurationInvalid, "apptest: no snapshot")
	}
	return c, nil
}

// SnapshotAge reports how stale the local view is.
func (p *ConfigProvider) SnapshotAge() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.Age
}

// Invalidate drops a merchant from the local view.
func (p *ConfigProvider) Invalidate(m shared.MerchantID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.snapshot, m)
}

// --- idempotency, outbox, ledger, audit ------------------------------------------------------------

// IdempotencyStore is the in-memory authoritative idempotency record.
type IdempotencyStore struct{ s *Store }

func idemKey(k ports.IdempotencyKey) string {
	return k.TenantID.String() + "|" + k.MerchantID.String() + "|" + k.Method + "|" + k.PathTemplate + "|" + k.Key
}

// Claim is an ON CONFLICT DO NOTHING insert, which is what makes two concurrent identical
// requests resolve deterministically rather than racing.
func (r *IdempotencyStore) Claim(ctx context.Context, rec ports.IdempotencyRecord) (ports.ClaimResult, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	k := idemKey(rec.Key)
	existing, ok := r.s.Idempotency[k]
	if !ok {
		r.s.Idempotency[k] = rec
		return ports.ClaimResult{Outcome: ports.ClaimNew}, nil
	}
	if existing.Fingerprint != "" && rec.Fingerprint != "" && existing.Fingerprint != rec.Fingerprint {
		return ports.ClaimResult{Outcome: ports.ClaimFingerprintMismatch}, nil
	}
	return ports.ClaimResult{Outcome: ports.ClaimInProgress, RetryAfter: time.Second}, nil
}

func (r *IdempotencyStore) Complete(ctx context.Context, key ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	return nil
}

func (r *IdempotencyStore) FailTerminal(ctx context.Context, key ports.IdempotencyKey, snap ports.ResponseSnapshot) error {
	return nil
}

func (r *IdempotencyStore) Release(ctx context.Context, key ports.IdempotencyKey) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	delete(r.s.Idempotency, idemKey(key))
	return nil
}

func (r *IdempotencyStore) PurgeExpired(ctx context.Context, before time.Time, limit int) (int, error) {
	return 0, nil
}

// OutboxWriter appends to the transactional outbox.
type OutboxWriter struct {
	s   *Store
	rec *Recorder
}

// Append writes the messages inside the caller's transaction.
func (w *OutboxWriter) Append(ctx context.Context, msgs ...ports.OutboxMessage) error {
	w.s.mu.Lock()
	defer w.s.mu.Unlock()
	w.s.Outbox = append(w.s.Outbox, msgs...)
	for range msgs {
		w.rec.Record("outbox.append")
	}
	return nil
}

// LedgerRepo appends balanced transactions.
type LedgerRepo struct{ s *Store }

// Append stores a transaction, re-checking that it balances. The real repository relies on a
// database CHECK constraint for the same thing; enforcing it here keeps the double honest.
func (r *LedgerRepo) Append(ctx context.Context, tx *ledger.Transaction) error {
	if err := tx.Balance(); err != nil {
		return err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Ledger = append(r.s.Ledger, tx)
	return nil
}

// Balance folds every entry for the account.
func (r *LedgerRepo) Balance(ctx context.Context, key ledger.AccountKey) (money.Money, error) {
	if err := key.Validate(); err != nil {
		return money.Money{}, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	acct, err := ledger.NewAccount(key, fixedClock{})
	if err != nil {
		return money.Money{}, err
	}
	for _, tx := range r.s.Ledger {
		for _, e := range tx.Entries() {
			if e.AccountKey() != key {
				continue
			}
			if acct, err = acct.Apply(e); err != nil {
				return money.Money{}, err
			}
		}
	}
	return acct.Balance(), nil
}

// EntriesForPayment returns every entry touching a payment.
func (r *LedgerRepo) EntriesForPayment(ctx context.Context, id shared.PaymentID) ([]ledger.Entry, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []ledger.Entry
	for _, tx := range r.s.Ledger {
		for _, e := range tx.Entries() {
			if e.PaymentID() == id {
				out = append(out, e)
			}
		}
	}
	return out, nil
}

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Unix(0, 0).UTC() }

// AuditRepo is the in-memory hash chain.
type AuditRepo struct{ s *Store }

func (r *AuditRepo) Append(ctx context.Context, rec audit.Record) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Audit = append(r.s.Audit, rec)
	return nil
}

func (r *AuditRepo) LastDigest(ctx context.Context, t shared.TenantID) (string, int64, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if len(r.s.Audit) == 0 {
		return "", 0, nil
	}
	last := r.s.Audit[len(r.s.Audit)-1]
	return last.Digest(), int64(len(r.s.Audit)), nil
}

func (r *AuditRepo) Query(ctx context.Context, f ports.AuditFilter, page ports.Page) ([]audit.Record, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return append([]audit.Record(nil), r.s.Audit...), "", nil
}

func (r *AuditRepo) VerifyRange(ctx context.Context, t shared.TenantID, from, to int64) (bool, int64, error) {
	return true, 0, nil
}

// --- webhooks, workflows, reconciliation -----------------------------------------------------------

// WebhookRepo is the in-memory inbound webhook store. Its Record method is the deduplication
// point, exactly as the real one's unique index is.
type WebhookRepo struct{ s *Store }

// NewWebhookRecorder exposes the same in-memory store as the ingress's untenanted recorder.
//
// The accept path writes through ports.WebhookRecorder rather than a unit of work, because a
// delivery arrives before its tenant is knowable. A test that wired only a unit of work would
// exercise a path production does not have, so this constructor exists to keep the two the same
// shape rather than to save a line.
func NewWebhookRecorder(s *Store) ports.WebhookRecorder { return &WebhookRepo{s: s} }

// Record stores the webhook, returning false when the (gateway, event id) pair has been seen.
func (r *WebhookRepo) Record(ctx context.Context, w ports.InboundWebhook) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	k := w.GatewayID.String() + "|" + w.GatewayEventID
	if _, seen := r.s.WebhookSeen[k]; seen {
		return false, nil
	}
	cp := w
	r.s.WebhookSeen[k] = w.ID
	r.s.Webhooks[w.ID] = &cp
	return true, nil
}

func (r *WebhookRepo) Get(ctx context.Context, id shared.WebhookID) (*ports.InboundWebhook, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.Webhooks[id]
	if !ok {
		return nil, apierror.New(apierror.CodeWebhookUnknownEventType, "apptest: webhook not found")
	}
	return w, nil
}

func (r *WebhookRepo) ClaimUnprocessed(ctx context.Context, limit int) ([]ports.InboundWebhook, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []ports.InboundWebhook
	for _, w := range r.s.Webhooks {
		if w.ProcessedAt == nil {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *WebhookRepo) MarkProcessed(ctx context.Context, id shared.WebhookID, result string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.Webhooks[id]
	if !ok {
		return apierror.New(apierror.CodeWebhookUnknownEventType, "apptest: webhook not found")
	}
	now := time.Now().UTC()
	w.ProcessedAt = &now
	w.Status = result
	return nil
}

func (r *WebhookRepo) MarkFailed(ctx context.Context, id shared.WebhookID, err error, retryAt time.Time) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	w, ok := r.s.Webhooks[id]
	if !ok {
		return apierror.New(apierror.CodeWebhookUnknownEventType, "apptest: webhook not found")
	}
	w.Attempts++
	if err != nil {
		w.LastError = err.Error()
	}
	return nil
}

// WorkflowRepo is the in-memory workflow store.
type WorkflowRepo struct{ s *Store }

func (r *WorkflowRepo) CreateInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	k := i.Definition + "|" + i.BusinessKey
	if _, live := r.s.WorkflowKey[k]; live {
		return apierror.New(apierror.CodeOnboardingAlreadyInProgress, "apptest: a live instance exists")
	}
	cp := i
	r.s.Workflows[i.ID] = &cp
	r.s.WorkflowKey[k] = i.ID
	return nil
}

func (r *WorkflowRepo) GetInstance(ctx context.Context, id shared.WorkflowID) (*ports.WorkflowInstanceRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	i, ok := r.s.Workflows[id]
	if !ok {
		return nil, apierror.New(apierror.CodeWorkflowNotFound, "apptest: instance not found")
	}
	return i, nil
}

func (r *WorkflowRepo) GetInstanceByBusinessKey(ctx context.Context, def, key string) (*ports.WorkflowInstanceRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	id, ok := r.s.WorkflowKey[def+"|"+key]
	if !ok {
		return nil, apierror.New(apierror.CodeWorkflowNotFound, "apptest: instance not found")
	}
	return r.s.Workflows[id], nil
}

func (r *WorkflowRepo) LeaseRunnable(ctx context.Context, workerID string, lease time.Duration, limit int) ([]ports.WorkflowInstanceRecord, error) {
	return nil, nil
}

func (r *WorkflowRepo) Heartbeat(ctx context.Context, id shared.WorkflowID, workerID string, epoch int64, extend time.Duration) error {
	return nil
}

func (r *WorkflowRepo) SaveInstance(ctx context.Context, i ports.WorkflowInstanceRecord) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	cp := i
	r.s.Workflows[i.ID] = &cp
	return nil
}

func (r *WorkflowRepo) SaveStep(ctx context.Context, st ports.WorkflowStepRecord) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	steps := r.s.Steps[st.WorkflowID]
	for i := range steps {
		if steps[i].Name == st.Name {
			steps[i] = st
			r.s.Steps[st.WorkflowID] = steps
			return nil
		}
	}
	r.s.Steps[st.WorkflowID] = append(steps, st)
	return nil
}

func (r *WorkflowRepo) ListSteps(ctx context.Context, id shared.WorkflowID) ([]ports.WorkflowStepRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := append([]ports.WorkflowStepRecord(nil), r.s.Steps[id]...)
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func (r *WorkflowRepo) PushDLQ(ctx context.Context, id shared.WorkflowID, step string, payload []byte, reason string) error {
	return nil
}

func (r *WorkflowRepo) CountByState(ctx context.Context) (map[string]int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := map[string]int{}
	for _, i := range r.s.Workflows {
		out[i.State]++
	}
	return out, nil
}

func (r *WorkflowRepo) FindStuck(ctx context.Context, noProgressFor time.Duration, limit int) ([]ports.WorkflowInstanceRecord, error) {
	return nil, nil
}

// ReconciliationRepo is the in-memory exception queue.
type ReconciliationRepo struct{ s *Store }

func (r *ReconciliationRepo) OpenException(ctx context.Context, e ports.ReconciliationException) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.Exceptions = append(r.s.Exceptions, e)
	return nil
}

func (r *ReconciliationRepo) ListOpen(ctx context.Context, severity string, page ports.Page) ([]ports.ReconciliationException, string, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	return append([]ports.ReconciliationException(nil), r.s.Exceptions...), "", nil
}

func (r *ReconciliationRepo) Resolve(ctx context.Context, id, resolution, by string) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for i := range r.s.Exceptions {
		if r.s.Exceptions[i].ID == id {
			now := time.Now().UTC()
			r.s.Exceptions[i].ResolvedAt = &now
			r.s.Exceptions[i].Resolution = resolution
			r.s.Exceptions[i].ResolvedBy = by
		}
	}
	return nil
}

func (r *ReconciliationRepo) CountOpen(ctx context.Context) (map[string]int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	out := map[string]int{}
	for _, e := range r.s.Exceptions {
		if e.ResolvedAt == nil {
			out[e.Severity]++
		}
	}
	return out, nil
}

// ErrNotImplemented marks a double method a test has not needed yet. It is an error rather than a
// panic so that an unexpected call fails one test rather than the process.
var ErrNotImplemented = errors.New("apptest: not implemented")

// Payment returns the stored payment, or nil. Reading through the store rather than through the
// service is what lets an assertion describe what was actually committed rather than what the
// service chose to return.
func (s *Store) Payment(id shared.PaymentID) *payment.Payment {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Payments[id]
	if !ok {
		return nil
	}
	return row.p
}

// AllPayments returns every stored payment, ordered by identifier.
func (s *Store) AllPayments() []*payment.Payment {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*payment.Payment, 0, len(s.Payments))
	for _, row := range s.Payments {
		out = append(out, row.p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// PutPayment seeds a payment directly, for tests that start from an already-authorized state.
func (s *Store) PutPayment(p *payment.Payment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Payments[p.ID()] = &paymentRow{p: p, version: p.Version()}
	p.DrainEvents()
}

// PutMerchant seeds a merchant.
func (s *Store) PutMerchant(m *merchant.Merchant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Merchants[m.ID()] = &merchantRow{m: m, version: m.Version()}
	m.DrainEvents()
}

// PutConnection seeds a gateway connection.
func (s *Store) PutConnection(c *gateway.Connection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Connections[c.ID()] = c
}

// PutTenant seeds a tenant.
func (s *Store) PutTenant(t *tenant.Tenant) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tenants[t.ID()] = t
}

// PutGateway seeds a gateway descriptor.
func (s *Store) PutGateway(g *gateway.Gateway) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Gateways[g.ID()] = g
}

// PutHealth seeds a health record.
func (s *Store) PutHealth(h *gateway.Health) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Health[healthKey(h.GatewayID(), h.Operation())] = h
}

// OutboxTypes returns the event types written to the outbox, in order. It is the assertion
// behind "the state change and its event commit together": a rolled-back transaction leaves
// neither, and a committed one leaves both.
func (s *Store) OutboxTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.Outbox))
	for _, m := range s.Outbox {
		out = append(out, m.Type)
	}
	return out
}

// OpenExceptions returns the unresolved reconciliation exceptions.
func (s *Store) OpenExceptions() []ports.ReconciliationException {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []ports.ReconciliationException
	for _, e := range s.Exceptions {
		if e.ResolvedAt == nil {
			out = append(out, e)
		}
	}
	return out
}

// Merchant returns the stored merchant, or nil.
func (s *Store) Merchant(id shared.MerchantID) *merchant.Merchant {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.Merchants[id]
	if !ok {
		return nil
	}
	return row.m
}

// AllMerchants returns every stored merchant, ordered by identifier.
func (s *Store) AllMerchants() []*merchant.Merchant {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*merchant.Merchant, 0, len(s.Merchants))
	for _, row := range s.Merchants {
		out = append(out, row.m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Gateway returns the stored descriptor, or nil.
func (s *Store) Gateway(id shared.GatewayID) *gateway.Gateway {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Gateways[id]
}

// AllGateways returns every stored descriptor, ordered by slug.
func (s *Store) AllGateways() []*gateway.Gateway {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*gateway.Gateway, 0, len(s.Gateways))
	for _, g := range s.Gateways {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// PostingLog is the in-memory deduplication record a consumer claims against before writing its
// effect.
//
// It writes through the store inside the caller's transaction, which is the property that matters:
// a claim committed separately from the effect it guards is a claim that lies, and the rollback
// test asserts that a failed effect leaves the reference unclaimed.
type PostingLog struct{ Store *Store }

// Claim records that a reference has been handled, reporting false if it already had been.
func (l *PostingLog) Claim(ctx context.Context, r ports.Repositories, group, reference string) (bool, error) {
	l.Store.mu.Lock()
	defer l.Store.mu.Unlock()
	if l.Store.Claims == nil {
		l.Store.Claims = map[string]struct{}{}
	}
	k := group + "|" + reference
	if _, seen := l.Store.Claims[k]; seen {
		return false, nil
	}
	l.Store.Claims[k] = struct{}{}
	return true, nil
}

// LedgerTransactions returns the appended transactions, in order.
func (s *Store) LedgerTransactions() []*ledger.Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*ledger.Transaction(nil), s.Ledger...)
}
