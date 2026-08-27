package postgres

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/udaykishore-resu/payments-platform/internal/domain/ledger"
	"github.com/udaykishore-resu/payments-platform/internal/domain/merchant"
	"github.com/udaykishore-resu/payments-platform/internal/domain/payment"
	"github.com/udaykishore-resu/payments-platform/migrations"
)

// TestMigrationsLoadAndAreWellFormed is the whole-set structural check: contiguous numbering,
// every up paired with a down, stable checksums.
func TestMigrationsLoadAndAreWellFormed(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("the shipped migration set does not load: %v", err)
	}
	if len(set) == 0 {
		t.Fatal("no migrations found")
	}
	for i, m := range set {
		if m.Version != i+1 {
			t.Fatalf("migration %d is numbered %04d; numbering must be contiguous from 0001",
				i, m.Version)
		}
		if strings.TrimSpace(m.Up) == "" {
			t.Fatalf("%s has an empty up script", m)
		}
		if strings.TrimSpace(m.Down) == "" {
			t.Fatalf("%s has an empty down script", m)
		}
		if len(m.Checksum) != 64 {
			t.Fatalf("%s has a malformed checksum", m)
		}
	}
}

// TestNoUnmarkedDropTableInAnUpMigration.
//
// A DROP TABLE in an up migration is irreversible data loss for which no compensating migration
// can exist — the down script's CREATE TABLE gives the table back empty, which for `payments` is
// the difference between a schema rollback and an incident. The marker is not permission to do
// it lightly; it is a requirement that the author write down, in the file, that they know.
func TestNoUnmarkedDropTableInAnUpMigration(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	dropRe := regexp.MustCompile(`(?i)\bDROP\s+(TABLE|COLUMN|SCHEMA)\b`)

	for _, m := range set {
		lines := strings.Split(m.Up, "\n")
		for i, line := range lines {
			if !dropRe.MatchString(line) {
				continue
			}
			if !precededByIrreversibleMarker(lines, i) {
				t.Errorf("%s line %d performs a destructive DROP with no `-- IRREVERSIBLE:` "+
					"marker above it:\n  %s", m, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// precededByIrreversibleMarker looks back over the comment block immediately above a line.
// The marker has to be adjacent — a marker fifty lines up in a file header would let one
// acknowledged DROP authorise every later one.
func precededByIrreversibleMarker(lines []string, at int) bool {
	for i := at - 1; i >= 0 && i >= at-6; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.Contains(trimmed, "IRREVERSIBLE:") {
			return true
		}
		if trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			return false
		}
	}
	return false
}

// TestEveryTenantScopedTableHasForcedRLS is the test that catches the *next* migration that
// forgets a policy — which is the whole reason it exists, and why skipping it is not an option.
//
// It works from the DDL text rather than from a live catalog, so it runs in CI with no database.
// The integration suite asserts the same property against information_schema, where a policy that
// exists in a file but failed to apply would also be caught.
func TestEveryTenantScopedTableHasForcedRLS(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all := strings.Join(upScripts(set), "\n")

	createRe := regexp.MustCompile(`(?is)CREATE TABLE (?:IF NOT EXISTS )?pp\.([a-z_]+) \((.*?)\n\)`)
	for _, match := range createRe.FindAllStringSubmatch(all, -1) {
		table, body := match[1], match[2]

		// Tables with no tenant_id are platform-global by design and are enumerated here so that
		// adding a new one is a deliberate edit to this list rather than an omission nobody sees.
		if !strings.Contains(body, "tenant_id") {
			if !platformGlobalTables[table] {
				t.Errorf("pp.%s has no tenant_id and is not on the platform-global allowlist; "+
					"either add the column or justify the exception in this test", table)
			}
			continue
		}
		if platformGlobalTables[table] {
			continue
		}

		for _, required := range []string{
			fmt.Sprintf("ALTER TABLE pp.%s ENABLE ROW LEVEL SECURITY", table),
			fmt.Sprintf("ALTER TABLE pp.%s FORCE  ROW LEVEL SECURITY", table),
			fmt.Sprintf("CREATE POLICY tenant_isolation ON pp.%s", table),
		} {
			if !strings.Contains(all, required) {
				t.Errorf("pp.%s is tenant-scoped but the migrations never issue:\n  %s",
					table, required)
			}
		}

		// USING alone is not enough. A policy with only USING lets tenant A *insert* a row
		// stamped with tenant B's identifier: A cannot read it back, but it is there, it appears
		// in B's queries, and it corrupts B's data.
		policy := policyBlockFor(all, table)
		if policy == "" {
			continue
		}
		if !strings.Contains(policy, "USING") {
			t.Errorf("the policy on pp.%s has no USING clause", table)
		}
		if !strings.Contains(policy, "WITH CHECK") {
			t.Errorf("the policy on pp.%s has no WITH CHECK clause; without it a tenant can "+
				"write rows stamped with another tenant's identifier", table)
		}
		// The `true` is missing_ok. Without it an unset GUC raises an exception; with it the
		// expression is NULL, the comparison is NULL, NULL is not TRUE, and the result is zero
		// rows. Failing closed with zero rows is the single most important property here.
		if !strings.Contains(policy, "current_setting('app.tenant_id', true)") {
			t.Errorf("the policy on pp.%s does not use current_setting('app.tenant_id', true); "+
				"the missing_ok argument is what makes an unset GUC fail closed", table)
		}
	}
}

// platformGlobalTables are the tables that legitimately carry no tenant_id.
//
// Each one is a deliberate exception with a stated reason, and the list is short on purpose:
// every addition widens the set of rows RLS does not protect.
var platformGlobalTables = map[string]bool{
	// The gateway catalog: every tenant sees the same descriptors, and per-tenant copies drift.
	"gateways": true,
	// Health is per (gateway, operation) and never per merchant — a per-merchant sample is too
	// sparse to be meaningful, and a gateway that is down is down for everyone.
	"gateway_health": true,
	// The role catalog: the scope vocabulary is identical for every tenant.
	"roles": true,
	// Reference data: ISO currency and payment-method tables, read-only to the app role.
	"currencies":      true,
	"payment_methods": true,
	// The FSM tables consulted by the guard triggers, and the migration/partition bookkeeping.
	"payment_state_transitions":   true,
	"merchant_status_transitions": true,
	"schema_migrations":           true,
	"partition_registry":          true,
	// Dedup claims are keyed by gateway event and by consumer group; tenancy is unknown at the
	// moment the claim is made, which is the point of making the claim first.
	"webhook_dedup": true,
	"event_dedup":   true,
}

func policyBlockFor(sql, table string) string {
	marker := fmt.Sprintf("CREATE POLICY tenant_isolation ON pp.%s", table)
	i := strings.Index(sql, marker)
	if i < 0 {
		return ""
	}
	rest := sql[i:]
	if end := strings.Index(rest, ";"); end > 0 {
		return rest[:end]
	}
	return rest
}

// TestTransitionTablesMatchDomain is the check that makes the database a genuine second line of
// defence rather than a comment.
//
// The trigger in migration 0013 consults a table of legal transitions. If that table drifts from
// the domain's state machine, one of two things happens and both are bad: the database permits a
// transition the domain forbids (and the guard is decoration), or it forbids one the domain
// permits (and a legitimate payment fails at 3 a.m. with a check violation). Comparing the two
// sets exactly is the only way to know.
func TestTransitionTablesMatchDomain(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all := strings.Join(upScripts(set), "\n")

	t.Run("payment", func(t *testing.T) {
		t.Parallel()
		want := map[string]bool{}
		for _, e := range payment.Machine().Edges() {
			want[string(e.From)+"->"+string(e.To)] = true
		}
		got := parseTransitionSeed(t, all, "pp.payment_state_transitions")
		assertSameEdges(t, "payment", want, got)
	})

	t.Run("merchant", func(t *testing.T) {
		t.Parallel()
		want := map[string]bool{}
		for _, e := range merchant.Machine().Edges() {
			want[string(e.From)+"->"+string(e.To)] = true
		}
		got := parseTransitionSeed(t, all, "pp.merchant_status_transitions")
		assertSameEdges(t, "merchant", want, got)
	})
}

var transitionRowRe = regexp.MustCompile(`\(\s*'([A-Z_]+)'\s*,\s*'([A-Z_]+)'\s*\)`)

func parseTransitionSeed(t *testing.T, sql, table string) map[string]bool {
	t.Helper()
	marker := "INSERT INTO " + table
	i := strings.Index(sql, marker)
	if i < 0 {
		t.Fatalf("no seed INSERT found for %s", table)
	}
	rest := sql[i:]
	end := strings.Index(rest, ";")
	if end < 0 {
		t.Fatalf("unterminated INSERT for %s", table)
	}
	out := map[string]bool{}
	for _, m := range transitionRowRe.FindAllStringSubmatch(rest[:end], -1) {
		out[m[1]+"->"+m[2]] = true
	}
	if len(out) == 0 {
		t.Fatalf("%s seed parsed to zero rows", table)
	}
	return out
}

func assertSameEdges(t *testing.T, machine string, want, got map[string]bool) {
	t.Helper()
	var missing, extra []string
	for e := range want {
		if !got[e] {
			missing = append(missing, e)
		}
	}
	for e := range got {
		if !want[e] {
			extra = append(extra, e)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("the %s transition table in migration 0013 is missing edges the domain "+
			"permits, so legitimate transitions will be refused by the database: %v",
			machine, missing)
	}
	if len(extra) > 0 {
		t.Errorf("the %s transition table in migration 0013 permits edges the domain does not, "+
			"so the database guard is decoration for those: %v", machine, extra)
	}
}

// TestLoadMigrationsRejectsMalformedSets drives the loader with the mistakes that actually
// happen: a gap left by a merge, an up with no down, a bad filename.
func TestLoadMigrationsRejectsMalformedSets(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{
			name: "gap in numbering",
			fsys: fstest.MapFS{
				"0001_a.up.sql":   {Data: []byte("SELECT 1")},
				"0001_a.down.sql": {Data: []byte("SELECT 1")},
				"0003_c.up.sql":   {Data: []byte("SELECT 1")},
				"0003_c.down.sql": {Data: []byte("SELECT 1")},
			},
			want: "not contiguous",
		},
		{
			name: "up with no down",
			fsys: fstest.MapFS{
				"0001_a.up.sql": {Data: []byte("SELECT 1")},
			},
			want: "no down script",
		},
		{
			name: "down with no up",
			fsys: fstest.MapFS{
				"0001_a.down.sql": {Data: []byte("SELECT 1")},
			},
			want: "no up script",
		},
		{
			name: "two names for one version",
			fsys: fstest.MapFS{
				"0001_a.up.sql":   {Data: []byte("SELECT 1")},
				"0001_b.down.sql": {Data: []byte("SELECT 1")},
			},
			want: "two different names",
		},
		{
			name: "missing direction",
			fsys: fstest.MapFS{
				"0001_a.sql": {Data: []byte("SELECT 1")},
			},
			want: "up.sql",
		},
		{
			name: "non-numeric version",
			fsys: fstest.MapFS{
				"abcd_a.up.sql": {Data: []byte("SELECT 1")},
			},
			want: "non-numeric version",
		},
		{
			name: "zero version",
			fsys: fstest.MapFS{
				"0000_a.up.sql": {Data: []byte("SELECT 1")},
			},
			want: "non-numeric version",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := LoadMigrations(tc.fsys)
			if err == nil {
				t.Fatalf("expected a rejection mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestChecksumIsStableAndContentSensitive: the checksum is what catches an edit to an
// already-applied migration, which is the most common way a staging schema and a production
// schema silently diverge.
func TestChecksumIsStableAndContentSensitive(t *testing.T) {
	t.Parallel()

	base := fstest.MapFS{
		"0001_a.up.sql":   {Data: []byte("CREATE TABLE pp.x ();")},
		"0001_a.down.sql": {Data: []byte("-- IRREVERSIBLE: test fixture\nDROP TABLE pp.x;")},
	}
	first, err := LoadMigrations(base)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	second, err := LoadMigrations(base)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if first[0].Checksum != second[0].Checksum {
		t.Fatal("the checksum must be stable across loads")
	}

	edited := fstest.MapFS{
		"0001_a.up.sql":   {Data: []byte("CREATE TABLE pp.x (y INT);")},
		"0001_a.down.sql": {Data: []byte("-- IRREVERSIBLE: test fixture\nDROP TABLE pp.x;")},
	}
	third, err := LoadMigrations(edited)
	if err != nil {
		t.Fatalf("load edited: %v", err)
	}
	if third[0].Checksum == first[0].Checksum {
		t.Fatal("editing an up migration must change its checksum")
	}
}

// TestNoBypassRLSAnywhere. BYPASSRLS makes every policy on every table inert for that role,
// silently and globally — the RLS layer would still exist in the schema, still pass a naive
// audit, and protect nothing.
func TestNoBypassRLSAnywhere(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, m := range set {
		// Comments are stripped first. Both 0001 and 0012 explain at length *why* BYPASSRLS must
		// not appear, and a test that cannot tell an explanation from a GRANT would force those
		// explanations to be deleted — which is the opposite of what this check is protecting.
		body := stripSQLComments(m.Up) + "\n" + stripSQLComments(m.Down)
		// NOBYPASSRLS and NOSUPERUSER are the *correct* spellings and must not trip this.
		scrubbed := strings.ReplaceAll(body, "NOBYPASSRLS", "")
		scrubbed = strings.ReplaceAll(scrubbed, "NOSUPERUSER", "")
		for _, forbidden := range []string{"BYPASSRLS", "SUPERUSER", "SECURITY DEFINER"} {
			if strings.Contains(scrubbed, forbidden) {
				t.Errorf("%s contains %q; see docs/multi-tenancy.md section 2.1 for why none of "+
					"these may appear", m, forbidden)
			}
		}
	}
}

// stripSQLComments removes line comments so that prose about a forbidden construct is not
// mistaken for the construct itself.
func stripSQLComments(sql string) string {
	lines := strings.Split(sql, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestPaymentAndAttemptSharePartitionKey guards amendment A-02 at the DDL level.
//
// If payment_attempts were ever partitioned on its own timestamp instead of the payment's,
// invariant I3's per-partition index would stop constraining the full set — silently, with no
// error at write time and no symptom until a double charge.
func TestPaymentAndAttemptSharePartitionKey(t *testing.T) {
	// Verifies: NFR-15.
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all := strings.Join(upScripts(set), "\n")

	for _, table := range []string{"payments", "payment_attempts"} {
		want := fmt.Sprintf("CREATE TABLE pp.%s (", table)
		if !strings.Contains(all, want) {
			t.Fatalf("no CREATE TABLE for pp.%s", table)
		}
	}
	if strings.Count(all, "PARTITION BY RANGE (partition_month)") < 4 {
		t.Fatalf("payments, payment_attempts, ledger_entries and audit_records must all be "+
			"partitioned on partition_month; found %d",
			strings.Count(all, "PARTITION BY RANGE (partition_month)"))
	}
	if !strings.Contains(all, "REFERENCES pp.payments (payment_id, partition_month)") {
		t.Fatal("payment_attempts must carry a foreign key on (payment_id, partition_month); " +
			"it is what makes a wrong partition month unwritable, and therefore what makes " +
			"invariant I3 hold across the whole set")
	}
	if !strings.Contains(all, `(payment_id) WHERE outcome = ''SUCCESS''`) {
		t.Fatal("the I3 partial unique index must be created per partition by " +
			"pp.create_partition; without it a partition permits two successful attempts")
	}
}

func upScripts(set []Migration) []string {
	out := make([]string, 0, len(set))
	for _, m := range set {
		out = append(out, m.Up)
	}
	return out
}

// TestEnumChecksMatchTheDomain asserts that every CHECK (col IN (...)) list in the schema is
// exactly the domain's universe for that type.
//
// Enums are TEXT + CHECK rather than native PostgreSQL enums (04-domain-model.md §6.0), which
// makes adding a value a one-line migration — and makes forgetting to add it a silent divergence
// instead of a compile error. This test is the compile error.
//
// The two failure directions are both bad and both invisible without it: a value the domain has
// and the CHECK does not means legitimate writes fail with a check violation at 3 a.m., and a
// value the CHECK has and the domain does not means a row this binary cannot rehydrate.
func TestEnumChecksMatchTheDomain(t *testing.T) {
	t.Parallel()

	set, err := LoadMigrations(migrations.Files())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	all := strings.Join(upScripts(set), "\n")

	cases := []struct {
		name   string
		column string
		want   []string
	}{
		{"payment state", "state", stringUniverse(payment.Machine().States())},
		{"attempt outcome", "outcome", stringUniverse(payment.AttemptMachine().States())},
		{"refund status", "status", stringUniverse(payment.RefundMachine().States())},
		{"merchant status", "status", stringUniverse(merchant.Machine().States())},
		{"ledger account type", "account_type", stringUniverse(ledger.AllAccountTypes())},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !containsEveryValueInSomeCheck(all, tc.want) {
				t.Fatalf("no CHECK list in the migrations contains exactly the %s universe %v; "+
					"the schema and the domain have diverged", tc.name, tc.want)
			}
		})
	}
}

// containsEveryValueInSomeCheck looks for one CHECK list that is a superset of want and reports
// whether every value appears in it. It matches on the set rather than on the literal text so a
// reformatting of the SQL does not break the test.
func containsEveryValueInSomeCheck(sql string, want []string) bool {
	quoted := make([]string, len(want))
	for i, v := range want {
		quoted[i] = "'" + v + "'"
	}
	for _, block := range checkListRe.FindAllString(sql, -1) {
		ok := true
		for _, q := range quoted {
			if !strings.Contains(block, q) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		// Superset is not enough: a CHECK carrying a value the domain does not know would let a
		// row exist that no binary can rehydrate.
		if strings.Count(block, "'")/2 != len(want) {
			continue
		}
		return true
	}
	return false
}

var checkListRe = regexp.MustCompile(`(?s)CHECK \([a-z_]+ IN \([^)]*\)\)`)

func stringUniverse[T ~string](in []T) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, string(v))
	}
	sort.Strings(out)
	return out
}
