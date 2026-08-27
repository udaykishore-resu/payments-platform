package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	"github.com/udaykishore-resu/payments-platform/internal/domain/audit"
	"github.com/udaykishore-resu/payments-platform/internal/domain/config"
	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
)

// exploding is a querier that fails the test if it is ever used.
//
// It is the whole mechanism of this file: the requirement is not "a call without a tenant
// returns an error", it is "a call without a tenant returns an error **without querying**"
// (R-TX-5, baseline §16.2). Querying anyway would appear to work — RLS evaluates an unset GUC to
// NULL and returns zero rows — and the caller would report a 404 for what is actually a
// missing-authentication bug. A mock that merely returns an error could not tell the two apart;
// one that explodes can.
type exploding struct{ t *testing.T }

func (e exploding) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	e.t.Helper()
	e.t.Fatal("a repository issued a statement without a tenant in context")
	return pgconn.CommandTag{}, nil
}

func (e exploding) Query(context.Context, string, ...any) (pgx.Rows, error) {
	e.t.Helper()
	e.t.Fatal("a repository issued a statement without a tenant in context")
	return nil, nil //nolint:nilnil // unreachable: t.Fatal above ends the goroutine, and this return exists only to satisfy pgx's interface
}

func (e exploding) QueryRow(context.Context, string, ...any) pgx.Row {
	e.t.Helper()
	e.t.Fatal("a repository issued a statement without a tenant in context")
	return nil
}

// withTenant installs a resolver that reports the given tenant for the duration of one test.
//
// It is not parallel-safe (the resolver is process state), so every test in this file is
// sequential. That is a fair price: the alternative is threading a resolver through fourteen
// constructors to make one guard testable.
func withTenant(t *testing.T, id string) {
	t.Helper()
	prev := tenantFrom
	t.Cleanup(func() { tenantFrom = prev })
	if id == "" {
		UseTenantResolver(nil)
		return
	}
	UseTenantResolver(func(context.Context) (string, bool) { return id, true })
}

const guardTenant = shared.TenantID("ten_01JB8Z9K2QW3E4R5T6Y7U8I9O0")

// TestRepositoriesRefuseToQueryWithoutATenant walks every repository method that takes a
// tenant-scoped path and asserts it returns MISSING_TENANT_CONTEXT before touching the querier.
func TestRepositoriesRefuseToQueryWithoutATenant(t *testing.T) {
	// Verifies: FR-06, NFR-29.
	withTenant(t, "") // no tenant anywhere

	q := exploding{t: t}
	ctx := context.Background()
	clock := shared.SystemClock{}

	outbox := &OutboxRepository{q: q, tenant: guardTenant, clock: clock}
	pay := &PaymentRepository{q: q, tenant: guardTenant, clock: clock, outbox: outbox}
	merch := &MerchantRepository{q: q, tenant: guardTenant, clock: clock}
	ten := &TenantRepository{q: q, tenant: guardTenant}
	gw := &GatewayRepository{q: q}
	conn := &ConnectionRepository{q: q, tenant: guardTenant}
	health := &HealthRepository{q: q, clock: clock}
	cfg := &ConfigRepository{q: q, tenant: guardTenant}
	idem := &IdempotencyStore{q: q, tenant: guardTenant, clock: clock}
	led := &LedgerRepository{q: q, tenant: guardTenant, clock: clock}
	aud := &AuditRepository{q: q, tenant: guardTenant}
	hooks := &WebhookRepository{q: q, tenant: guardTenant, clock: clock}
	wf := &WorkflowRepository{q: q, tenant: guardTenant, clock: clock}
	recon := &ReconciliationRepository{q: q, tenant: guardTenant, clock: clock}
	dedup := NewDedupStore(q, clock, 0)

	calls := map[string]func() error{
		"payments.Get":                      func() error { _, e := pay.Get(ctx, "pay_x"); return e },
		"payments.GetForUpdate":             func() error { _, e := pay.GetForUpdate(ctx, "pay_x"); return e },
		"payments.List":                     func() error { _, _, e := pay.List(ctx, ports.PaymentFilter{}, ports.Page{}); return e },
		"payments.FindUnresolved":           func() error { _, e := pay.FindUnresolved(ctx, time.Minute, 10); return e },
		"payments.FindExpiredAuthorization": func() error { _, e := pay.FindExpiredAuthorizations(ctx, time.Now(), 10); return e },
		"payments.CountOpen":                func() error { _, e := pay.CountOpen(ctx, "mrc_x"); return e },

		"merchants.Get":             func() error { _, e := merch.Get(ctx, "mrc_x"); return e },
		"merchants.GetForUpdate":    func() error { _, e := merch.GetForUpdate(ctx, "mrc_x"); return e },
		"merchants.GetByExternal":   func() error { _, e := merch.GetByExternalRef(ctx, "ref"); return e },
		"merchants.List":            func() error { _, _, e := merch.List(ctx, ports.MerchantFilter{}, ports.Page{}); return e },
		"merchants.FindKYCExpiring": func() error { _, e := merch.FindKYCExpiring(ctx, time.Hour, 10); return e },

		"tenants.Get":          func() error { _, e := ten.Get(ctx, guardTenant); return e },
		"tenants.GetAPIClient": func() error { _, e := ten.GetAPIClient(ctx, "cli_x"); return e },

		"gateways.Get":  func() error { _, e := gw.Get(ctx, "stripe"); return e },
		"gateways.List": func() error { _, e := gw.List(ctx); return e },

		"connections.Get":               func() error { _, e := conn.Get(ctx, "gwc_x"); return e },
		"connections.GetByMerchantGW":   func() error { _, e := conn.GetByMerchantGateway(ctx, "mrc_x", "stripe"); return e },
		"connections.ListForMerchant":   func() error { _, e := conn.ListForMerchant(ctx, "mrc_x"); return e },
		"connections.FindRotatableCred": func() error { _, e := conn.FindCredentialsDueForRotation(ctx, time.Hour, 10); return e },

		"health.Get":     func() error { _, e := health.Get(ctx, "stripe", shared.OpAuthorize); return e },
		"health.ListAll": func() error { _, e := health.ListAll(ctx); return e },

		"configs.GetActive":       func() error { _, e := cfg.GetActive(ctx, "mrc_x"); return e },
		"configs.GetVersion":      func() error { _, e := cfg.GetVersion(ctx, "mrc_x", 1); return e },
		"configs.ListVersions":    func() error { _, _, e := cfg.ListVersions(ctx, "mrc_x", ports.Page{}); return e },
		"configs.ListActiveSince": func() error { _, e := cfg.ListActiveSince(ctx, time.Now(), 10); return e },
		"configs.Publish": func() error {
			return cfg.Publish(ctx, &config.MerchantConfig{TenantID: guardTenant}, 0)
		},

		"idempotency.Claim": func() error {
			_, e := idem.Claim(ctx, ports.IdempotencyRecord{
				Key: ports.IdempotencyKey{TenantID: guardTenant, Method: "POST", PathTemplate: "/v1/payments", Key: "k"},
			})
			return e
		},
		"idempotency.Complete": func() error {
			return idem.Complete(ctx, ports.IdempotencyKey{TenantID: guardTenant}, ports.ResponseSnapshot{})
		},
		"idempotency.FailTerminal": func() error {
			return idem.FailTerminal(ctx, ports.IdempotencyKey{TenantID: guardTenant}, ports.ResponseSnapshot{})
		},
		"idempotency.Release": func() error {
			return idem.Release(ctx, ports.IdempotencyKey{TenantID: guardTenant})
		},
		"idempotency.PurgeExpired": func() error { _, e := idem.PurgeExpired(ctx, time.Now(), 10); return e },

		"outbox.Append": func() error {
			return outbox.Append(ctx, ports.OutboxMessage{TenantID: guardTenant, PartitionKey: "k"})
		},
		"outbox.Claim":         func() error { _, e := outbox.Claim(ctx, 0, 1, 10); return e },
		"outbox.MarkPublished": func() error { return outbox.MarkPublished(ctx, []shared.EventID{"evt_x"}) },
		"outbox.MarkFailed":    func() error { return outbox.MarkFailed(ctx, "evt_x", nil, time.Now()) },
		"outbox.Backlog":       func() error { _, e := outbox.Backlog(ctx); return e },

		"dedup.MarkProcessed": func() error { _, e := dedup.MarkProcessed(ctx, "g", "evt_x"); return e },

		"ledger.Balance": func() error {
			_, e := led.Balance(ctx, ledger.AccountKey{
				TenantID: guardTenant, MerchantID: "mrc_x",
				Type: ledger.AccountMerchantReceivable, Currency: "USD",
			})
			return e
		},
		"ledger.EntriesForPayment": func() error { _, e := led.EntriesForPayment(ctx, "pay_x"); return e },

		"audit.Append":      func() error { return aud.Append(ctx, audit.Record{}) },
		"audit.LastDigest":  func() error { _, _, e := aud.LastDigest(ctx, guardTenant); return e },
		"audit.Query":       func() error { _, _, e := aud.Query(ctx, ports.AuditFilter{}, ports.Page{}); return e },
		"audit.VerifyRange": func() error { _, _, e := aud.VerifyRange(ctx, guardTenant, 1, 2); return e },

		"webhooks.Record":           func() error { _, e := hooks.Record(ctx, ports.InboundWebhook{}); return e },
		"webhooks.Get":              func() error { _, e := hooks.Get(ctx, "whk_x"); return e },
		"webhooks.ClaimUnprocessed": func() error { _, e := hooks.ClaimUnprocessed(ctx, 10); return e },
		"webhooks.MarkProcessed":    func() error { return hooks.MarkProcessed(ctx, "whk_x", "ok") },
		"webhooks.MarkFailed":       func() error { return hooks.MarkFailed(ctx, "whk_x", nil, time.Now()) },

		"workflows.CreateInstance": func() error {
			return wf.CreateInstance(ctx, ports.WorkflowInstanceRecord{TenantID: guardTenant})
		},
		"workflows.GetInstance":     func() error { _, e := wf.GetInstance(ctx, "wfr_x"); return e },
		"workflows.GetByBusinesKey": func() error { _, e := wf.GetInstanceByBusinessKey(ctx, "d", "k"); return e },
		"workflows.LeaseRunnable":   func() error { _, e := wf.LeaseRunnable(ctx, "w", time.Minute, 10); return e },
		"workflows.Heartbeat":       func() error { return wf.Heartbeat(ctx, "wfr_x", "w", 1, time.Minute) },
		"workflows.SaveInstance": func() error {
			return wf.SaveInstance(ctx, ports.WorkflowInstanceRecord{TenantID: guardTenant})
		},
		"workflows.SaveStep": func() error {
			return wf.SaveStep(ctx, ports.WorkflowStepRecord{TenantID: guardTenant})
		},
		"workflows.ListSteps":    func() error { _, e := wf.ListSteps(ctx, "wfr_x"); return e },
		"workflows.PushDLQ":      func() error { return wf.PushDLQ(ctx, "wfr_x", "s", nil, "r") },
		"workflows.CountByState": func() error { _, e := wf.CountByState(ctx); return e },
		"workflows.FindStuck":    func() error { _, e := wf.FindStuck(ctx, time.Hour, 10); return e },

		"reconciliation.OpenException": func() error {
			return recon.OpenException(ctx, ports.ReconciliationException{TenantID: guardTenant})
		},
		"reconciliation.ListOpen":  func() error { _, _, e := recon.ListOpen(ctx, "", ports.Page{}); return e },
		"reconciliation.Resolve":   func() error { return recon.Resolve(ctx, "id", "r", "me") },
		"reconciliation.CountOpen": func() error { _, e := recon.CountOpen(ctx); return e },
	}

	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatalf("%s returned nil with no tenant in context", name)
			}
			if got := apierror.CodeOf(err); got != apierror.CodeMissingTenantContext {
				t.Fatalf("%s returned %s, want MISSING_TENANT_CONTEXT (%v)", name, got, err)
			}
		})
	}
}

// TestRepositoriesRefuseAMismatchedTenant covers the second half of the guard: a context whose
// tenant differs from the one the transaction was opened for. That can only happen if a context
// crossed a transaction boundary, and continuing would run the statement under the transaction's
// GUC while the caller believed they were reading someone else's data — silently wrong rather
// than empty.
func TestRepositoriesRefuseAMismatchedTenant(t *testing.T) {
	withTenant(t, "ten_SOMEONEELSE")

	q := exploding{t: t}
	ctx := context.Background()
	pay := &PaymentRepository{q: q, tenant: guardTenant, clock: shared.SystemClock{}}

	if _, err := pay.Get(ctx, "pay_x"); apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("Get with a mismatched tenant = %v, want TENANT_MISMATCH", err)
	}
	if _, _, err := pay.List(ctx, ports.PaymentFilter{}, ports.Page{}); apierror.CodeOf(err) != apierror.CodeTenantMismatch {
		t.Fatalf("List with a mismatched tenant = %v, want TENANT_MISMATCH", err)
	}
}

// TestDefaultResolverFailsClosed. Until main wires a resolver, the persistence layer must refuse
// everything — a misconfigured binary should be loudly wrong, never quietly permissive.
func TestDefaultResolverFailsClosed(t *testing.T) {
	withTenant(t, "")
	if _, err := tenantOf(context.Background()); apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
		t.Fatalf("the default resolver must report no tenant; got %v", err)
	}
	UseTenantResolver(nil) // explicitly nil must not panic and must stay closed
	if _, err := tenantOf(context.Background()); err == nil {
		t.Fatal("UseTenantResolver(nil) must restore the fail-closed default, not open the gate")
	}
}
