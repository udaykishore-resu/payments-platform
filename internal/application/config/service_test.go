package config

import (
	"context"
	"testing"
	"time"

	"github.com/udaykishore-resu/payments-platform/internal/application/apptest"
	"github.com/udaykishore-resu/payments-platform/internal/application/ports"
	dconfig "github.com/udaykishore-resu/payments-platform/internal/domain/config"
	dgateway "github.com/udaykishore-resu/payments-platform/internal/domain/gateway"
	"github.com/udaykishore-resu/payments-platform/internal/domain/risk"
	"github.com/udaykishore-resu/payments-platform/internal/domain/routing"
	"github.com/udaykishore-resu/payments-platform/internal/domain/shared"
	"github.com/udaykishore-resu/payments-platform/pkg/apierror"
	"github.com/udaykishore-resu/payments-platform/pkg/money"
)

var testEpoch = time.Date(2026, 3, 14, 9, 0, 0, 0, time.UTC)

const (
	testTenant   shared.TenantID   = "ten_01HZTESTTENANT00000000000"
	testMerchant shared.MerchantID = "mrc_01HZTESTMERCHANT000000000"
	gwA          shared.GatewayID  = "gw-a"
	gwB          shared.GatewayID  = "gw-b"
)

type env struct {
	t     *testing.T
	store *apptest.Store
	clock *apptest.Clock
	audit *apptest.Auditor
	svc   *Service
}

func newEnv(t *testing.T) *env {
	t.Helper()
	store := apptest.NewStore()
	clock := apptest.NewClock(testEpoch)
	a := &apptest.Auditor{Store: store}
	store.PutGateway(descriptor(t, gwA, clock))
	store.PutGateway(descriptor(t, gwB, clock))
	return &env{
		t: t, store: store, clock: clock, audit: a,
		svc: NewService(Deps{
			UoW:   apptest.NewUnitOfWork(store, apptest.NewRecorder()),
			Audit: a, Clock: clock, Gateways: catalog{store: store},
		}),
	}
}

// catalog reads descriptors straight out of the store, which is what the registry does in
// production: the descriptors are the same rows the control plane publishes.
type catalog struct{ store *apptest.Store }

func (c catalog) Get(_ context.Context, g shared.GatewayID) (*dgateway.Gateway, error) {
	desc := c.store.Gateway(g)
	if desc == nil {
		return nil, apierror.Newf(apierror.CodeGatewayNotConfigured, "no descriptor for %s", g)
	}
	return desc, nil
}

func (c catalog) List(context.Context) ([]*dgateway.Gateway, error) {
	return c.store.AllGateways(), nil
}

func descriptor(t *testing.T, id shared.GatewayID, clock shared.Clock) *dgateway.Gateway {
	t.Helper()
	costs, err := dgateway.NewCostModel(dgateway.CostRate{
		Currency: "EUR", Method: dgateway.AnyMethod,
		FixedFee: money.MustNew(25, "EUR"), BasisPoints: 290,
	})
	if err != nil {
		t.Fatalf("NewCostModel: %v", err)
	}
	g, err := dgateway.NewGateway(dgateway.NewGatewayParams{
		ID: id, DisplayName: "Gateway " + id.String(),
		SignatureScheme: dgateway.SchemeHMACSHA256,
		BaseURLs:        map[shared.Environment]string{shared.EnvironmentSandbox: "https://x.example"},
		Capabilities: dgateway.Capabilities{
			Countries:  []shared.Country{"DE"},
			Currencies: []money.Currency{"EUR"},
			Methods:    []shared.PaymentMethod{shared.MethodCard},
			Operations: []shared.Operation{shared.OpAuthorize, shared.OpCapture, shared.OpRefund},
			// 180 days, which is what makes the refund-window cross-check meaningful below.
			MaxRefundWindow: 180 * 24 * time.Hour,
		},
		CostModel: costs,
	}, clock)
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	return g
}

func draft() *dconfig.MerchantConfig {
	return &dconfig.MerchantConfig{
		MerchantID: testMerchant, TenantID: testTenant,
		Environment:         shared.EnvironmentSandbox,
		SupportedCurrencies: []money.Currency{"EUR"},
		PaymentMethods:      []shared.PaymentMethod{shared.MethodCard},
		Countries:           []shared.Country{"DE"},
		Routing: routing.Policy{
			Strategy: routing.StrategyPriorityWithFallback,
			Primary:  gwA, Fallbacks: []shared.GatewayID{gwB},
			ConnectedGateways: []shared.GatewayID{gwA, gwB},
		},
		Risk: risk.Policy{
			MaxTransactionAmount: money.MustNew(500000, "EUR"),
			Require3DSAbove:      money.MustNew(10000, "EUR"),
			DailyVolumeLimit:     money.MustNew(5000000, "EUR"),
		},
		Limits: dconfig.Limits{MaxRefundWindowDays: 180, MaxPartialCaptures: 4},
	}
}

func publish(t *testing.T, e *env, d *dconfig.MerchantConfig, ifMatch, comment string) (*dconfig.MerchantConfig, error) {
	t.Helper()
	return e.svc.Publish(context.Background(), PublishCommand{
		TenantID: testTenant, MerchantID: testMerchant, Draft: d,
		IfMatch: ifMatch, Comment: comment, Actor: Actor{ID: "usr_1", Name: "Operator"},
	})
}

// TestFirstPublishNeedsNoPrecondition and produces version 1.
//
// Requiring an If-Match on a merchant that has no configuration would make bootstrapping
// impossible, and there is no concurrent edit to lose.
func TestFirstPublishNeedsNoPrecondition(t *testing.T) {
	// Verifies: FR-44.
	t.Parallel()
	e := newEnv(t)
	c, err := publish(t, e, draft(), "", "initial configuration")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if c.Version != 1 {
		t.Fatalf("version = %d, want 1", c.Version)
	}
	if c.Status != dconfig.StatusActive {
		t.Fatalf("status = %s, want ACTIVE", c.Status)
	}
}

// TestPublishIsAppendOnly: the previous version stays readable, superseded rather than replaced.
func TestPublishIsAppendOnly(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first, err := publish(t, e, draft(), "", "initial")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	next := draft()
	next.Limits.MaxPartialCaptures = 2
	second, err := publish(t, e, next, first.ETag(), "tighten partial captures")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}
	if second.Version != 2 || second.PreviousVersion != 1 {
		t.Fatalf("version chain = %d ← %d, want 2 ← 1", second.Version, second.PreviousVersion)
	}

	old, err := e.svc.GetVersion(context.Background(), testTenant, testMerchant, 1)
	if err != nil {
		t.Fatalf("GetVersion(1): %v", err)
	}
	if old.Limits.MaxPartialCaptures != 4 {
		t.Fatalf("version 1 was mutated: maxPartialCaptures = %d, want 4", old.Limits.MaxPartialCaptures)
	}
}

// TestPublishRequiresAndHonoursIfMatch.
func TestPublishRequiresAndHonoursIfMatch(t *testing.T) {
	// Verifies: BR-33, FR-44.
	t.Parallel()
	e := newEnv(t)
	first, err := publish(t, e, draft(), "", "initial")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if _, err := publish(t, e, draft(), "", "no precondition"); err == nil {
		t.Fatal("a replacement publish with no If-Match was accepted")
	}

	if _, err := publish(t, e, draft(), first.ETag(), "second"); err != nil {
		t.Fatalf("Publish with a fresh ETag: %v", err)
	}
	// The now-stale tag must be refused rather than silently overwrite the version above.
	_, err = publish(t, e, draft(), first.ETag(), "third")
	if err == nil {
		t.Fatal("a stale If-Match was accepted")
	}
	if apierror.CodeOf(err) != apierror.CodeConfigurationVersionConflict {
		t.Fatalf("got %s, want CONFIGURATION_VERSION_CONFLICT", apierror.CodeOf(err))
	}
}

// TestPublishRunsL4AndRejectsTheCombinationDefects.
//
// These are the defects a field-by-field validator cannot see: each field is individually legal
// and the *combination* is wrong. They publish cleanly under any per-field scheme and fail at
// three in the morning.
func TestPublishRunsL4AndRejectsTheCombinationDefects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*dconfig.MerchantConfig)
	}{
		{
			name: "a 3DS threshold above the transaction ceiling can never fire",
			mutate: func(c *dconfig.MerchantConfig) {
				c.Risk.Require3DSAbove = money.MustNew(900000, "EUR")
			},
		},
		{
			name: "a daily limit below the per-transaction limit breaches itself on one payment",
			mutate: func(c *dconfig.MerchantConfig) {
				c.Risk.DailyVolumeLimit = money.MustNew(1000, "EUR")
			},
		},
		{
			name:   "a refund window longer than the gateway will honour",
			mutate: func(c *dconfig.MerchantConfig) { c.Limits.MaxRefundWindowDays = 365 },
		},
		{
			name: "a currency no routed gateway supports",
			mutate: func(c *dconfig.MerchantConfig) {
				c.SupportedCurrencies = []money.Currency{"EUR", "JPY"}
			},
		},
		{
			name: "a method no routed gateway supports",
			mutate: func(c *dconfig.MerchantConfig) {
				c.PaymentMethods = []shared.PaymentMethod{shared.MethodCard, shared.MethodUPI}
			},
		},
		{
			name:   "a fallback chain that is empty under a strategy whose purpose is to fail over",
			mutate: func(c *dconfig.MerchantConfig) { c.Routing.Fallbacks = nil },
		},
	}
	for _, tc := range tests {

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			d := draft()
			tc.mutate(d)
			if _, err := publish(t, e, d, "", "should fail"); err == nil {
				t.Fatalf("%s was published", tc.name)
			}
			if _, err := e.svc.GetActive(context.Background(), testTenant, testMerchant); err == nil {
				t.Fatal("a rejected publish left an active configuration behind")
			}
		})
	}
}

// TestPublishRequiresAComment. A configuration history with no reasons is a list of diffs nobody
// can interpret six months later, which is when it is read.
func TestPublishRequiresAComment(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := publish(t, e, draft(), "", ""); err == nil {
		t.Fatal("a publish with no comment was accepted")
	}
}

// TestPublishAuditsWithAJSONPatchDiffAndEmitsTheEvent.
//
// The diff is stored rather than the two documents because it is what a human reads and what a
// query can aggregate: "which publishes touched routing.primary" is answerable from patches and
// is an archaeology project from snapshots.
func TestPublishAuditsWithAJSONPatchDiffAndEmitsTheEvent(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	first, err := publish(t, e, draft(), "", "initial")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	next := draft()
	next.Routing.Primary = gwB
	next.Routing.Fallbacks = []shared.GatewayID{gwA}
	if _, err := publish(t, e, next, first.ETag(), "move primary to gw-b"); err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	line := e.store.AuditLines[len(e.store.AuditLines)-1]
	if line.Action != "configuration.published" {
		t.Fatalf("audit action = %q", line.Action)
	}
	patch, ok := line.Detail["patch"].([]PatchOp)
	if !ok {
		t.Fatalf("the audit record carries no patch: %#v", line.Detail["patch"])
	}
	if !hasPath(patch, "/routing/Primary") && !hasPath(patch, "/routing/primary") {
		t.Fatalf("the patch does not mention the primary gateway: %#v", patch)
	}

	types := e.store.OutboxTypes()
	if len(types) != 2 || types[1] != EventConfigurationPublished {
		t.Fatalf("outbox = %v, want two configuration.published.v1 events", types)
	}
}

// TestRollbackIsForwardOnly.
//
// Deleting the bad version would erase the fact that anybody ever published it — the single most
// interesting thing about the incident — and would leave every data-plane replica pointing at a
// version number that no longer exists.
func TestRollbackIsForwardOnly(t *testing.T) {
	// Verifies: BR-33, FR-46.
	t.Parallel()
	e := newEnv(t)
	v1, err := publish(t, e, draft(), "", "initial")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	bad := draft()
	bad.Limits.MaxPartialCaptures = 1
	v2, err := publish(t, e, bad, v1.ETag(), "reduce partial captures")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	v3, err := e.svc.Rollback(context.Background(), RollbackCommand{
		TenantID: testTenant, MerchantID: testMerchant, ToVersion: 1,
		IfMatch: v2.ETag(), Actor: Actor{ID: "usr_2"},
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if v3.Version != 3 {
		t.Fatalf("rollback produced version %d, want 3 (forward-only)", v3.Version)
	}
	if v3.Limits.MaxPartialCaptures != 4 {
		t.Fatalf("the rolled-back content is wrong: maxPartialCaptures = %d, want 4",
			v3.Limits.MaxPartialCaptures)
	}
	// Every version, including the one rolled back from, is still readable.
	for v := 1; v <= 3; v++ {
		if _, err := e.svc.GetVersion(context.Background(), testTenant, testMerchant, v); err != nil {
			t.Fatalf("version %d is no longer readable: %v", v, err)
		}
	}
	if !containsAction(e.audit.Actions(), "configuration.rolled_back") {
		t.Fatalf("audit = %v, want a rolled_back record", e.audit.Actions())
	}
}

// TestRollbackRefusesAForwardTarget: rolling "back" to a later version is not a rollback.
func TestRollbackRefusesAForwardTarget(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	v1, err := publish(t, e, draft(), "", "initial")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := e.svc.Rollback(context.Background(), RollbackCommand{
		TenantID: testTenant, MerchantID: testMerchant, ToVersion: 1,
		IfMatch: v1.ETag(), Actor: Actor{ID: "u"},
	}); err == nil {
		t.Fatal("a rollback to the active version was accepted")
	}
}

// TestEveryEntryPointAssertsTenantContext.
func TestEveryEntryPointAssertsTenantContext(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	calls := map[string]func() error{
		"GetActive":  func() error { _, err := e.svc.GetActive(context.Background(), "", testMerchant); return err },
		"GetVersion": func() error { _, err := e.svc.GetVersion(context.Background(), "", testMerchant, 1); return err },
		"ListVersions": func() error {
			_, _, err := e.svc.ListVersions(context.Background(), "", testMerchant, ports.Page{})
			return err
		},
		"Publish": func() error {
			_, err := e.svc.Publish(context.Background(), PublishCommand{MerchantID: testMerchant, Draft: draft(), Comment: "x"})
			return err
		},
		"Rollback": func() error {
			_, err := e.svc.Rollback(context.Background(), RollbackCommand{MerchantID: testMerchant, ToVersion: 1})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Fatalf("%s accepted a request with no tenant context", name)
		}
		if apierror.CodeOf(err) != apierror.CodeMissingTenantContext {
			t.Fatalf("%s: got %s, want MISSING_TENANT_CONTEXT", name, apierror.CodeOf(err))
		}
	}
}

// TestCapabilityLookupFailsClosedOnAnUnreadableDescriptor.
//
// Promising a refund window on a gateway nobody can describe produces a failure when a customer
// is owed money, which is the worst possible moment to discover a configuration defect.
func TestCapabilityLookupFailsClosedOnAnUnreadableDescriptor(t *testing.T) {
	t.Parallel()
	l := NewCapabilityLookup(context.Background(), emptyCatalog{})
	if l.CanRefundAfter(gwA, 24*time.Hour) {
		t.Fatal("an unreadable descriptor answered yes to a refund-window question")
	}
	if l.AnySupports([]shared.GatewayID{gwA}, "EUR", shared.MethodCard) {
		t.Fatal("an unreadable descriptor answered yes to a capability question")
	}
}

// TestDiffIsStableAndScalarPathed.
//
// Stable, because an audit record whose content depends on map iteration order cannot be compared
// with itself. Scalar-pathed, because "routing changed" is exactly as useful as reporting nothing.
func TestDiffIsStableAndScalarPathed(t *testing.T) {
	// Verifies: FR-45.
	t.Parallel()
	from := draft()
	to := draft()
	to.Limits.MaxPartialCaptures = 2

	first := Diff(from, to)
	second := Diff(from, to)
	if len(first) != len(second) {
		t.Fatalf("two diffs of the same pair produced %d and %d ops", len(first), len(second))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("op %d differs between runs: %#v vs %#v", i, first[i], second[i])
		}
	}
	if len(first) != 1 {
		t.Fatalf("got %d ops for a one-field change: %#v", len(first), first)
	}
	if first[0].Op != "replace" {
		t.Fatalf("op = %q, want replace", first[0].Op)
	}

	// A first publish is an `add` for every field, which is the honest description of it.
	initial := Diff(nil, to)
	for _, op := range initial {
		if op.Op != "add" {
			t.Fatalf("a first publish produced a %q op at %s", op.Op, op.Path)
		}
	}
}

type emptyCatalog struct{}

func (emptyCatalog) Get(context.Context, shared.GatewayID) (*dgateway.Gateway, error) {
	return nil, apierror.New(apierror.CodeGatewayNotConfigured, "no descriptor")
}

func (emptyCatalog) List(context.Context) ([]*dgateway.Gateway, error) { return nil, nil }

func hasPath(ops []PatchOp, path string) bool {
	for _, o := range ops {
		if o.Path == path {
			return true
		}
	}
	return false
}

func containsAction(all []string, want string) bool {
	for _, a := range all {
		if a == want {
			return true
		}
	}
	return false
}
