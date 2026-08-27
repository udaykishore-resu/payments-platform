//go:build integration

package integration

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/udaykishore-resu/payments-platform/tests/testenv"
)

// Tenant isolation, asserted with the application guard deliberately removed.
//
// Verifies: baseline §16.2 (isolation guard), ADR-008 (pooled multi-tenancy with RLS),
// docs/testing.md §4.2, critical path CP-07.
//
// The positive test — "the repository returns tenant A's rows when the context says tenant A" —
// proves nothing about isolation: it passes identically whether RLS exists or not. The test that
// matters is this one: every statement below is issued as pp_app over a raw connection, with no
// repository, no tenantctx and no guard anywhere in the path. The only thing standing between
// tenant A's session and tenant B's rows is the database.
//
// Two properties are asserted for every tenant-scoped table:
//
//   - Reads return zero rows, not an error. Existence is not disclosed: a "forbidden" would tell
//     tenant A that the id exists in someone else's account, and enumerating ids is then a
//     reconnaissance primitive.
//   - Writes are refused. An UPDATE or DELETE against a foreign row affects zero rows because the
//     policy's USING clause filters it away; an INSERT claiming a foreign tenant is rejected
//     outright by the policy's WITH CHECK clause.
//
// Everything runs inside one transaction that is rolled back, with the session GUC switched from
// tenant B to tenant A partway through. Switching the GUC mid-transaction is what makes this
// possible without committing: RLS re-evaluates current_setting on every statement, so tenant A's
// view of a row tenant B inserted moments earlier in the same transaction is exactly the view it
// would have of a committed one — and nothing is left behind in the tables where DELETE is revoked.

// isolationCase is one tenant-scoped table and the smallest legal row it can hold.
type isolationCase struct {
	// table is the fully qualified name.
	table string
	// idCol is the column the probes address the seeded row by.
	idCol string
	// insert writes one row owned by tenant, creating any parent rows it needs, and returns the
	// value of idCol. sfx disambiguates rows within one test.
	insert func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error)
	// skipForeignInsert, when non-empty, explains why the WITH CHECK probe is not run against
	// this table. It is a string rather than a bool so the reason is in the code, not in a
	// commit message nobody will find.
	skipForeignInsert string
}

// prefixAudit is the audit record's id prefix. It is not in testenv because nothing else in the
// suite writes audit rows.
const prefixAudit = "aud"

// isolationCases is the behavioural matrix.
//
// Every tenant-scoped table in the schema must appear here or in
// tablesCoveredByCatalogAssertionOnly, and TestEveryTenantScopedTableIsCoveredByAnIsolationCase
// fails when a new one appears in neither — which is the day the missing isolation test is cheap
// to write.
var isolationCases = []isolationCase{
	{
		table: "pp.merchants",
		idCol: "merchant_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			id := s.ID(testenv.PrefixMerchant, "iso/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.merchants (merchant_id, tenant_id, legal_name, display_name, environment, status,
                          created_at, updated_at)
VALUES ($1,$2,'Isolation Probe','Isolation Probe','sandbox','CREATED',$3,$3)`,
				id, tenant, s.Clock.Now())
			return id, err
		},
	},
	{
		table: "pp.payments",
		idCol: "payment_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			p := s.NewPayment(tenant, merchant, "iso/"+sfx, 4_200)
			return p.ID, p.Insert(ctx, tx)
		},
	},
	{
		table: "pp.payment_attempts",
		idCol: "attempt_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			p := s.NewPayment(tenant, merchant, "iso/parent/"+sfx, 4_200)
			if err := p.Insert(ctx, tx); err != nil {
				return "", err
			}
			a := s.NewAttempt(p, "iso/att/"+sfx, 1, "PENDING")
			a.RequestSentAt = nil
			return a.ID, a.Insert(ctx, tx)
		},
		skipForeignInsert: "the row references a payment, and a foreign-tenant insert would be " +
			"refused by the foreign key before the policy's WITH CHECK is ever evaluated — the " +
			"probe would pass while asserting nothing about RLS",
	},
	{
		table: "pp.refunds",
		idCol: "refund_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			p := s.NewPayment(tenant, merchant, "iso/refparent/"+sfx, 4_200)
			if err := p.Insert(ctx, tx); err != nil {
				return "", err
			}
			id := s.ID(testenv.PrefixRefund, "iso/ref/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.refunds (refund_id, tenant_id, payment_id, partition_month, amount, currency,
                        reason, status, created_at, updated_at)
VALUES ($1,$2,$3,$4,100,'USD','OTHER','PENDING',$5,$5)`,
				id, tenant, p.ID, p.PartitionMonth, s.Clock.Now())
			return id, err
		},
		skipForeignInsert: "same foreign key to pp.payments as pp.payment_attempts",
	},
	{
		table: "pp.ledger_entries",
		idCol: "entry_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			at := s.Clock.Now()
			id := s.IDAt(testenv.PrefixLedger, at, "iso/led/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.ledger_entries (entry_id, partition_month, tenant_id, merchant_id, account_type,
                               side, amount, currency, entry_type, transaction_group_id, occurred_at)
VALUES ($1,$2,$3,$4,'GATEWAY_CLEARING','DEBIT',100,'USD','CAPTURE',$5,$6)`,
				id, testenv.PartitionMonth(at), tenant, merchant, "grp-iso-"+sfx, at)
			return id, err
		},
	},
	{
		table: "pp.outbox_events",
		idCol: "event_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			return s.InsertOutbox(ctx, tx, tenant, "payment.created.v1", "pp.payments.payment.v1",
				"pk-"+sfx, "agg-"+sfx, "iso/outbox/"+sfx)
		},
	},
	{
		table: "pp.idempotency_records",
		idCol: "idempotency_record_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			id := "req_iso_" + s.Nonce()[:8] + "_" + sfx
			_, err := tx.Exec(ctx, `
INSERT INTO pp.idempotency_records (
    idempotency_record_id, tenant_id, merchant_id, method, path_template, idempotency_key,
    request_fingerprint, state, lease_expires_at, expires_at)
VALUES ($1,$2,$3,'POST','/v1/payments',$4,$5,'IN_FLIGHT',$6,$6)`,
				id, tenant, merchant, "iso-"+sfx, fingerprintOf("iso/"+sfx),
				s.Clock.Now().Add(time.Hour))
			return id, err
		},
	},
	{
		table: "pp.inbound_webhooks",
		idCol: "webhook_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			id := s.ID(testenv.PrefixWebhook, "iso/whk/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.inbound_webhooks (webhook_id, tenant_id, gateway_id, gateway_event_id,
                                 raw_body, body_sha256)
VALUES ($1,$2,'simulator',$3,$4,$5)`,
				id, tenant, "evt-iso-"+s.Nonce()[:8]+"-"+sfx, []byte(`{"probe":true}`),
				fingerprintOf("iso-body-"+sfx))
			return id, err
		},
	},
	{
		table: "pp.workflow_instances",
		idCol: "instance_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			id := s.ID(testenv.PrefixWorkflow, "iso/wf/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.workflow_instances (instance_id, tenant_id, workflow_name, business_key,
                                   created_at, updated_at)
VALUES ($1,$2,'merchant_onboarding',$3,$4,$4)`,
				id, tenant, "iso-"+s.Nonce()[:8]+"-"+sfx, s.Clock.Now())
			return id, err
		},
	},
	{
		table: "pp.workflow_steps",
		idCol: "step_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			inst := s.ID(testenv.PrefixWorkflow, "iso/wfparent/"+sfx)
			if _, err := tx.Exec(ctx, `
INSERT INTO pp.workflow_instances (instance_id, tenant_id, workflow_name, business_key,
                                   created_at, updated_at)
VALUES ($1,$2,'merchant_onboarding',$3,$4,$4)`,
				inst, tenant, "isostep-"+s.Nonce()[:8]+"-"+sfx, s.Clock.Now()); err != nil {
				return "", err
			}
			id := s.ID(testenv.PrefixStep, "iso/step/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.workflow_steps (step_id, instance_id, tenant_id, name, sequence, state)
VALUES ($1,$2,$3,'validate-merchant',0,'PENDING')`, id, inst, tenant)
			return id, err
		},
		skipForeignInsert: "the row references a workflow instance; the foreign key fires before " +
			"the policy's WITH CHECK",
	},
	{
		table: "pp.reconciliation_exceptions",
		idCol: "exception_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			id := "exc_iso_" + s.Nonce()[:8] + "_" + sfx
			_, err := tx.Exec(ctx, `
INSERT INTO pp.reconciliation_exceptions (exception_id, tenant_id, merchant_id, kind, severity, detail)
VALUES ($1,$2,$3,'UNKNOWN_OUTCOME','MAJOR','isolation probe')`, id, tenant, merchant)
			return id, err
		},
	},
	{
		table: "pp.routing_plans",
		idCol: "routing_plan_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			id := s.ID(testenv.PrefixRoutingPln, "iso/rpl/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.routing_plans (routing_plan_id, tenant_id, merchant_id, candidates, decided_at)
VALUES ($1,$2,$3,'[]'::jsonb,$4)`, id, tenant, merchant, s.Clock.Now())
			return id, err
		},
	},
	{
		table: "pp.gateway_connections",
		idCol: "connection_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			// The gateway must exist: gateway_connections carries a foreign key to the registry,
			// which migration 0015 seeds. Reading the slug from the registry rather than naming
			// one keeps this probe working when the seeded set changes.
			var gatewayID string
			if err := tx.QueryRow(ctx,
				`SELECT gateway_id FROM pp.gateways ORDER BY gateway_id LIMIT 1`).Scan(&gatewayID); err != nil {
				return "", fmt.Errorf("read a seeded gateway: %w", err)
			}
			id := s.ID(testenv.PrefixConnection, "iso/gwc/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.gateway_connections (connection_id, tenant_id, merchant_id, gateway_id,
                                    environment, status, created_at, updated_at)
VALUES ($1,$2,$3,$4,'sandbox','UNPROVISIONED',$5,$5)`,
				id, tenant, merchant, gatewayID, s.Clock.Now())
			return id, err
		},
	},
	{
		table: "pp.audit_records",
		idCol: "audit_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, _, sfx string) (string, error) {
			at := s.Clock.Now()
			id := s.IDAt(prefixAudit, at, "iso/aud/"+sfx)
			_, err := tx.Exec(ctx, `
INSERT INTO pp.audit_records (audit_id, partition_month, tenant_id, sequence, actor_type, action,
                              outcome, occurred_at, recorded_at, prev_digest, entry_digest)
VALUES ($1,$2,$3,1,'SYSTEM','isolation.probe','SUCCESS',$4,$4,$5,$6)`,
				id, testenv.PartitionMonth(at), tenant, at,
				fingerprintOf("prev/"+sfx), fingerprintOf("entry/"+sfx))
			return id, err
		},
	},
	{
		table: "pp.payment_event_log",
		idCol: "payment_id",
		insert: func(ctx context.Context, tx pgx.Tx, s *testenv.Scope, tenant, merchant, sfx string) (string, error) {
			p := s.NewPayment(tenant, merchant, "iso/pel/"+sfx, 4_200)
			if err := p.Insert(ctx, tx); err != nil {
				return "", err
			}
			_, err := tx.Exec(ctx, `
INSERT INTO pp.payment_event_log (tenant_id, payment_id, partition_month, aggregate_version,
                                  from_state, to_state, trigger, occurred_at)
VALUES ($1,$2,$3,1,'','CREATED','create',$4)`,
				tenant, p.ID, p.PartitionMonth, s.Clock.Now())
			return p.ID, err
		},
	},
}

// tablesCoveredByCatalogAssertionOnly are the tenant-scoped tables whose isolation is asserted by
// the catalog check alone.
//
// Each entry is a deliberate omission with a reason, not a backlog. The rule for adding one is
// narrow: the table must be a control-plane registry with no money in it and no path from the data
// plane, so that the cost of building a legal row for it exceeds what the extra probe would tell
// us over the policy assertion that already covers every table.
var tablesCoveredByCatalogAssertionOnly = map[string]string{
	"pp.api_clients":               "authentication registry; written only by the control plane's client provisioning",
	"pp.certification_reports":     "sealed artifacts; the guard trigger makes a probe row unwritable without a full certification fixture",
	"pp.configurations":            "control-plane registry, reached only through the configuration publish path",
	"pp.configuration_versions":    "append-only version history behind a guard trigger",
	"pp.feature_flags":             "control-plane registry",
	"pp.gateway_credentials_meta":  "credential metadata; a probe row would need a live connection row and a secret reference",
	"pp.ledger_accounts":           "chart of accounts, seeded by migration rather than by the data plane",
	"pp.merchant_attestations":     "onboarding evidence, written only by the workflow",
	"pp.merchant_bank_accounts":    "onboarding evidence, written only by the workflow",
	"pp.merchant_business_profile": "onboarding evidence, written only by the workflow",
	"pp.merchant_principals":       "onboarding evidence, written only by the workflow",
	"pp.onboarding_cases":          "control-plane case registry, written only by the workflow",
	"pp.policies":                  "control-plane policy registry",
	"pp.reconciliation_runs":       "batch job bookkeeping, written only by the reconciler",
	"pp.role_bindings":             "authorization registry",
	"pp.workflow_dlq":              "parked workflow payloads; the instance it references is already covered",
	// pp.tenants cannot take the generic probe and gets a dedicated one instead. Its WITH CHECK
	// requires the row's own tenant_id to equal the session's, so there is no way to insert a
	// *second* tenant row under one tenant's session — the shape every other case relies on.
	// TestATenantCannotSeeAnotherTenantsRegistryRow covers it directly.
	"pp.tenants": "covered by TestATenantCannotSeeAnotherTenantsRegistryRow; its WITH CHECK makes " +
		"the generic seed-then-switch probe unconstructible",
}

// TestATenantCannotSeeAnotherTenantsRegistryRow is the pp.tenants case.
//
// Verifies: baseline §16.2, ADR-008. The tenant registry is the one table whose policy is
// self-referential — a session may only ever see and write its own row — so it needs a probe of its
// own rather than the generic one. Both of this scope's tenants already exist, seeded by
// testenv.Isolate, which is what makes the negative assertion meaningful: the row is definitely
// there and tenant A definitely cannot see it.
func TestATenantCannotSeeAnotherTenantsRegistryRow(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	err := s.Tenanted(c, s.TenantA, func(tx pgx.Tx) error {
		var own int64
		if err := tx.QueryRow(c,
			`SELECT count(*) FROM pp.tenants WHERE tenant_id = $1`, s.TenantA).Scan(&own); err != nil {
			return err
		}
		if own != 1 {
			return fmt.Errorf("tenant A cannot see its own registry row (%d rows); the probe below "+
				"would be vacuous", own)
		}

		var foreign int64
		if err := tx.QueryRow(c,
			`SELECT count(*) FROM pp.tenants WHERE tenant_id = $1`, s.TenantB).Scan(&foreign); err != nil {
			return err
		}
		if foreign != 0 {
			return fmt.Errorf("tenant A can see tenant B's registry row; the tenant table's own "+
				"policy is not isolating (%d rows)", foreign)
		}

		// And a session cannot plant a row for anyone else — the WITH CHECK half.
		_, insErr := tx.Exec(c, `
INSERT INTO pp.tenants (tenant_id, name, tier, status, residency_region, created_at, updated_at)
VALUES ($1,'forged','POOLED','ACTIVE','GLOBAL',$2,$2)`,
			s.ID(testenv.PrefixTenant, "forged"), s.Clock.Now())
		testenv.RequireDBRejection(t, insErr,
			"tenant A creating a tenant registry row for an identity that is not its own",
			testenv.SQLStateRLSViolation, testenv.SQLStateCheckViolation)
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// TestCrossTenantReadsAndWritesAreRefusedByTheDatabase is CP-07.
//
// Verifies: baseline §16.2, ADR-008, docs/testing.md §4.2. With tenant A's session GUC set and no
// application guard anywhere in the path, every read of a tenant B row returns zero rows and every
// write is refused by the database.
func TestCrossTenantReadsAndWritesAreRefusedByTheDatabase(t *testing.T) {
	t.Parallel()
	_, s := setup(t)

	for _, tc := range isolationCases {
		t.Run(tc.table, func(t *testing.T) {
			t.Parallel()
			c := ctx(t)

			err := s.Tenanted(c, s.TenantB, func(tx pgx.Tx) error {
				id, err := tc.insert(c, tx, s, s.TenantB, s.MerchantB, "probe")
				if err != nil {
					return fmt.Errorf("seed a tenant B row: %w", err)
				}

				// Sanity: the owner can see its own row. Without this, a probe that returned zero
				// rows for tenant A would be indistinguishable from a seed that silently wrote
				// nothing, and the test would "pass" while asserting nothing at all.
				var own int64
				if err := tx.QueryRow(c,
					`SELECT count(*) FROM `+tc.table+` WHERE `+tc.idCol+` = $1`, id).Scan(&own); err != nil {
					return fmt.Errorf("read back as the owner: %w", err)
				}
				if own != 1 {
					return fmt.Errorf("the owning tenant sees %d rows of its own seed, want 1; the "+
						"cross-tenant probe below would be vacuous", own)
				}

				// Become tenant A. Same connection, same transaction, different session GUC —
				// which is exactly the situation a pooled connection produces in production.
				if _, err := tx.Exec(c, `SELECT set_config('app.tenant_id', $1, true)`, s.TenantA); err != nil {
					return fmt.Errorf("switch the session GUC to tenant A: %w", err)
				}

				// 1. Read by primary key: zero rows, no error. Not a permission error — existence
				//    must not be disclosed.
				var seen int64
				if err := tx.QueryRow(c,
					`SELECT count(*) FROM `+tc.table+` WHERE `+tc.idCol+` = $1`, id).Scan(&seen); err != nil {
					return fmt.Errorf("tenant A's read raised an error instead of returning zero rows: %w", err)
				}
				if seen != 0 {
					return fmt.Errorf("RLS did not block the read: tenant A sees %d row(s) of %s "+
						"belonging to tenant B", seen, tc.table)
				}

				// 2. Read the whole table filtered on tenant B: still zero.
				var byTenant int64
				if err := tx.QueryRow(c,
					`SELECT count(*) FROM `+tc.table+` WHERE tenant_id = $1`, s.TenantB).Scan(&byTenant); err != nil {
					return fmt.Errorf("tenant A's tenant-filtered read raised an error: %w", err)
				}
				if byTenant != 0 {
					return fmt.Errorf("RLS did not block the read: tenant A sees %d of tenant B's "+
						"rows in %s when asking for them by tenant_id", byTenant, tc.table)
				}

				// 3. UPDATE: the policy's USING clause makes the row unmatched, so the statement
				//    affects nothing. Several tables go further — pp.ledger_entries and
				//    pp.audit_records revoke UPDATE outright, and the append-only tables answer
				//    with a trigger — and any of those is a stronger refusal.
				//
				//    Every write probe runs inside a savepoint. A refusal that arrives as an error
				//    rather than as zero rows aborts the transaction, and without the savepoint the
				//    next probe would fail with "current transaction is aborted" and the reader
				//    would chase the wrong table.
				if err := probeRefusesWrite(c, tx, "upd_probe",
					`UPDATE `+tc.table+` SET tenant_id = tenant_id WHERE `+tc.idCol+` = $1`,
					id, "update", tc.table); err != nil {
					return err
				}

				// 4. DELETE: same, and on the money tables DELETE is revoked entirely.
				if err := probeRefusesWrite(c, tx, "del_probe",
					`DELETE FROM `+tc.table+` WHERE `+tc.idCol+` = $1`,
					id, "delete", tc.table); err != nil {
					return err
				}

				// 5. INSERT claiming tenant B: refused by the policy's WITH CHECK clause. This is
				//    the half a USING-only policy would miss entirely — reads would be isolated
				//    while a tenant could still plant rows in another tenant's account.
				if tc.skipForeignInsert == "" {
					if _, err := tx.Exec(c, `SAVEPOINT ins_probe`); err != nil {
						return err
					}
					_, insErr := tc.insert(c, tx, s, s.TenantB, s.MerchantB, "foreign")
					testenv.RequireDBRejection(t, insErr,
						"tenant A inserting a row owned by tenant B into "+tc.table,
						testenv.SQLStateRLSViolation, testenv.SQLStateCheckViolation)
					if _, err := tx.Exec(c, `ROLLBACK TO SAVEPOINT ins_probe`); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("%s: %v", tc.table, err)
			}
		})
	}
}

// probeRefusesWrite issues one write against a foreign tenant's row and asserts the database
// refused it, one way or another.
//
// "One way or another" is the whole subtlety. RLS refuses by making the row invisible, so the
// statement succeeds and affects nothing. A revoked privilege refuses with 42501. An append-only
// trigger refuses with 23514. All three are correct answers to "tenant A must not write tenant B's
// row"; only "one or more rows affected" is wrong.
func probeRefusesWrite(c context.Context, tx pgx.Tx, savepoint, stmt, id, verb, table string) error {
	if _, err := tx.Exec(c, "SAVEPOINT "+savepoint); err != nil {
		return err
	}
	tag, err := tx.Exec(c, stmt, id)
	switch {
	case err == nil && tag.RowsAffected() != 0:
		return fmt.Errorf("tenant A performed a %s on %d row(s) of tenant B's data in %s",
			verb, tag.RowsAffected(), table)
	case err != nil:
		switch testenv.PgErrCode(err) {
		case testenv.SQLStateInsufficientPriv, testenv.SQLStateCheckViolation:
			// A privilege revoke or an append-only trigger. Both refuse harder than RLS does.
		default:
			return fmt.Errorf("tenant A's %s on %s failed with an unexpected error (SQLSTATE %s): %w",
				verb, table, testenv.PgErrCode(err), err)
		}
	}
	_, err = tx.Exec(c, "ROLLBACK TO SAVEPOINT "+savepoint)
	return err
}

// TestEveryTenantScopedTableIsCoveredByAnIsolationCase is the regression that keeps this file
// honest as the schema grows.
//
// Verifies: baseline §16.2. A new tenant-scoped table added without an isolation probe is a
// silent hole: the code that writes it will be reviewed, the policy will probably be copied from a
// neighbour, and nothing will ever confirm the copy was correct. This fails on the day the table
// appears, which is the only time adding the probe is cheap.
func TestEveryTenantScopedTableIsCoveredByAnIsolationCase(t *testing.T) {
	t.Parallel()
	pool, _ := setup(t)
	c := ctx(t)

	rows, err := pool.Query(c, `
SELECT 'pp.' || c.relname AS qualified,
       c.relrowsecurity,
       c.relforcerowsecurity,
       (SELECT count(*) FROM pg_policies p
         WHERE p.schemaname = 'pp' AND p.tablename = c.relname
           AND p.qual IS NOT NULL AND p.with_check IS NOT NULL) AS complete_policies
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'pp'
  AND c.relkind IN ('r', 'p')
  AND c.relispartition = false
  AND EXISTS (
        SELECT 1 FROM information_schema.columns col
         WHERE col.table_schema = 'pp' AND col.table_name = c.relname
           AND col.column_name = 'tenant_id')
ORDER BY 1`)
	if err != nil {
		t.Fatalf("enumerate tenant-scoped tables: %v", err)
	}
	defer rows.Close()

	covered := map[string]bool{}
	for _, tc := range isolationCases {
		covered[tc.table] = true
	}

	var uncovered, unprotected []string
	total := 0
	for rows.Next() {
		var name string
		var rls, force bool
		var policies int
		if err := rows.Scan(&name, &rls, &force, &policies); err != nil {
			t.Fatalf("scan: %v", err)
		}
		total++

		// The catalog half: every tenant-scoped table must have RLS enabled *and* forced, and at
		// least one policy carrying both clauses. FORCE is what stops the table owner — which is
		// the migration role, not pp_app, but the distinction is one `ALTER ROLE` away from being
		// wrong — from reading everything.
		if !rls || !force || policies == 0 {
			unprotected = append(unprotected, fmt.Sprintf(
				"%s (rls=%v force=%v policies-with-both-clauses=%d)", name, rls, force, policies))
		}
		if !covered[name] {
			if _, excused := tablesCoveredByCatalogAssertionOnly[name]; !excused {
				uncovered = append(uncovered, name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if total == 0 {
		t.Fatal("found no tenant-scoped tables at all; the catalog query is wrong and this test " +
			"has been passing without checking anything")
	}

	sort.Strings(unprotected)
	sort.Strings(uncovered)
	if len(unprotected) > 0 {
		t.Errorf("these tenant-scoped tables are not fully protected by RLS:\n  %s",
			strings.Join(unprotected, "\n  "))
	}
	if len(uncovered) > 0 {
		t.Errorf("these tenant-scoped tables have no isolation probe:\n  %s\n"+
			"Add an entry to isolationCases, or — if the table genuinely cannot be seeded cheaply "+
			"— add it to tablesCoveredByCatalogAssertionOnly with the reason.",
			strings.Join(uncovered, "\n  "))
	}
	t.Logf("%d tenant-scoped tables: %d with a behavioural probe, %d covered by the catalog "+
		"assertion alone", total, len(isolationCases), len(tablesCoveredByCatalogAssertionOnly))
}

// TestAStatementWithNoTenantInTheSessionSeesNothing is the fail-closed half.
//
// Verifies: baseline §16.2, R-TX-5. An unset `app.tenant_id` must produce zero rows rather than
// every row. The failure mode this guards against is a policy written as
// `tenant_id = coalesce(current_setting(...), tenant_id)`, which reads sensibly and grants a
// session with no tenant access to the entire table.
func TestAStatementWithNoTenantInTheSessionSeesNothing(t *testing.T) {
	t.Parallel()
	_, s := setup(t)
	c := ctx(t)

	// Seed under tenant B, then clear the GUC inside the same transaction.
	err := s.Tenanted(c, s.TenantB, func(tx pgx.Tx) error {
		p := s.NewPayment(s.TenantB, s.MerchantB, "no-tenant-probe", 1_500)
		if err := p.Insert(c, tx); err != nil {
			return err
		}
		if _, err := tx.Exec(c, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
			return err
		}
		for _, table := range []string{"pp.payments", "pp.merchants", "pp.outbox_events", "pp.ledger_entries"} {
			var n int64
			if err := tx.QueryRow(c, `SELECT count(*) FROM `+table).Scan(&n); err != nil {
				return fmt.Errorf("count %s with no tenant set: %w", table, err)
			}
			if n != 0 {
				return fmt.Errorf("%s returned %d rows to a session with no tenant; the policy is "+
					"fail-open", table, n)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}
