package rules_test

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/udaykishore-resu/payments-platform/internal/validation/engine"
	"github.com/udaykishore-resu/payments-platform/internal/validation/rules"
)

// This file is the contract between the code and docs/validation-plane.md.
//
// The reason it exists rather than a review convention: a rule that is implemented but not
// documented is a rejection a support engineer cannot explain, and a rule that is documented
// but not implemented is a promise the platform is not keeping. Both failures are silent, both
// are discovered by a customer, and both are trivially detectable by parsing one markdown file.

// catalogPath is the authoritative rule catalog, relative to this package.
const catalogPath = "../../../docs/validation-plane.md"

// unimplementedAllowlist names rules that appear in the catalog and deliberately do not exist
// in the registry.
//
// It is empty, and that is the intended steady state: every one of the 243 documented rules is
// implemented. The mechanism is kept because the alternative — a test that skips silently when
// the two sets diverge — makes divergence invisible, and because a rule genuinely withdrawn
// mid-flight needs somewhere to be recorded with a reason rather than being quietly deleted
// from the documentation. An entry here must carry the reason and the date it is expected to
// go away.
var unimplementedAllowlist = map[engine.RuleID]string{}

// docRule is one parsed catalog row.
type docRule struct {
	id          engine.RuleID
	level       int
	check       string
	severity    string
	code        string
	remediation string
	// pure is the catalog's Pure column ("Y"/"N"). For L7 the column is the database backstop
	// instead, and every L7 rule is pure by construction, so it is normalized to "Y".
	pure string
}

func TestEveryRuleIsDocumented(t *testing.T) {
	t.Parallel()
	doc := loadCatalog(t)

	var missing []string
	for _, r := range rules.Catalog() {
		d, ok := doc[r.ID]
		if !ok {
			missing = append(missing, string(r.ID)+" (no catalog row)")
			continue
		}
		if d.check == "" {
			missing = append(missing, string(r.ID)+" (catalog row has an empty check)")
		}
		if d.severity == "" {
			missing = append(missing, string(r.ID)+" (catalog row has an empty severity)")
		}
		if d.remediation == "" {
			missing = append(missing, string(r.ID)+" (catalog row has an empty remediation)")
		}
		// A warning legitimately has no error code; the catalog writes an em dash there.
		if d.severity == "E" && d.code == "" {
			missing = append(missing, string(r.ID)+" (ERROR row has an empty code)")
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d implemented rule(s) are not properly documented in %s:\n  %s",
			len(missing), catalogPath, strings.Join(missing, "\n  "))
	}
}

func TestEveryDocumentedRuleIsImplemented(t *testing.T) {
	t.Parallel()
	doc := loadCatalog(t)

	implemented := map[engine.RuleID]struct{}{}
	for _, r := range rules.Catalog() {
		implemented[r.ID] = struct{}{}
	}

	var missing []string
	for id := range doc {
		if _, ok := implemented[id]; ok {
			continue
		}
		if reason, allowed := unimplementedAllowlist[id]; allowed {
			t.Logf("allowlisted as unimplemented: %s (%s)", id, reason)
			continue
		}
		missing = append(missing, string(id))
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d documented rule(s) have no implementation and are not on the allowlist:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}

	// An allowlist entry for a rule that is in fact implemented is stale, and a stale allowlist
	// is how a permanent exemption grows out of a temporary one.
	for id := range unimplementedAllowlist {
		if _, ok := implemented[id]; ok {
			t.Errorf("%s is on the unimplemented allowlist but is implemented; remove the entry", id)
		}
		if _, ok := doc[id]; !ok {
			t.Errorf("%s is on the unimplemented allowlist but is not in the catalog either", id)
		}
	}
}

func TestNoDuplicateRuleIDs(t *testing.T) {
	t.Parallel()

	// The registry itself panics on a duplicate at init, so reaching this test at all proves
	// the implemented set is unique. What is still worth asserting is that the *catalog* does
	// not list an ID twice: two rows for one ID means two different assertions were documented
	// under one identifier, and only one of them is real.
	seen := map[engine.RuleID]int{}
	for _, line := range catalogLines(t) {
		if id, ok := ruleIDOfRow(line); ok {
			seen[id]++
		}
	}

	var dupes []string
	for id, n := range seen {
		if n > 1 {
			dupes = append(dupes, string(id))
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		t.Fatalf("rule ID(s) documented more than once: %s", strings.Join(dupes, ", "))
	}

	registered := map[engine.RuleID]struct{}{}
	for _, r := range rules.Catalog() {
		if _, ok := registered[r.ID]; ok {
			t.Fatalf("registry contains %s twice", r.ID)
		}
		registered[r.ID] = struct{}{}
	}
	if len(registered) != rules.Count() {
		t.Fatalf("registry reports %d rules but yielded %d unique IDs", rules.Count(), len(registered))
	}
}

func TestRuleIDsMatchLevelPrefix(t *testing.T) {
	t.Parallel()
	doc := loadCatalog(t)

	for _, r := range rules.Catalog() {
		if !r.ID.IsWellFormed() {
			t.Errorf("%s does not match ^L[1-7]\\.[A-Z0-9_]{4,60}$", r.ID)
			continue
		}
		level := r.ID.Level()
		if level < 1 || level > 7 {
			t.Errorf("%s carries level %d", r.ID, level)
			continue
		}
		d, ok := doc[r.ID]
		if !ok {
			continue // reported by TestEveryRuleIsDocumented
		}
		// The catalog is sectioned by level (### 3.1 L1 … ### 3.7 L7). A rule whose ID says L5
		// but which is documented in the L4 section has drifted, and every count, dashboard and
		// budget keyed on the level is wrong from then on.
		if d.level != level {
			t.Errorf("%s is documented in the L%d section but its ID says L%d", r.ID, d.level, level)
		}
	}
}

func TestHotPathRulesArePure(t *testing.T) {
	t.Parallel()
	doc := loadCatalog(t)

	// L5 and L7 admit no impure rule at all. They run inside the payment's 5 ms and 10 ms
	// budgets, and an impure rule there turns a bounded latency into an unbounded one and a
	// reproducible rejection into an irreproducible one.
	for _, level := range []int{5, 7} {
		for _, r := range rules.CatalogForLevel(level) {
			if !r.Pure {
				t.Errorf("%s is registered impure; no rule on L%d may be", r.ID, level)
			}
		}
	}

	// L1 and L6 each carry a small, enumerated set of impure rules that the catalog itself
	// documents with Pure = N: L1's are the CA and Redis lookups that sit outside the money
	// decision, and L6's is the webhook dedup-table read on ingress. They are permitted, and
	// the permission is derived from the catalog rather than restated here, so the moment
	// someone marks a *new* rule impure without documenting it the test fails.
	for _, level := range []int{1, 6} {
		for _, r := range rules.CatalogForLevel(level) {
			d, ok := doc[r.ID]
			if !ok {
				continue // reported by TestEveryRuleIsDocumented
			}
			docPure := d.pure != "N"
			switch {
			case !r.Pure && docPure:
				t.Errorf("%s is registered impure but the catalog documents it as pure; "+
					"a new impure rule on L%d needs an explicit catalog change", r.ID, level)
			case r.Pure && !docPure:
				t.Errorf("%s is registered pure but the catalog documents it as impure", r.ID)
			}
		}
	}

	// And the whole catalog's purity must agree with its documentation, at every level: purity
	// is what the promotion process, the budget test and the reproducibility guarantee are all
	// keyed on, so a declaration that disagrees with the documentation is a defect wherever it
	// occurs.
	for _, r := range rules.Catalog() {
		d, ok := doc[r.ID]
		if !ok || r.ID.Level() == 7 {
			continue
		}
		if want := d.pure != "N"; want != r.Pure {
			t.Errorf("%s: registered Pure=%v, catalog says Pure=%v", r.ID, r.Pure, want)
		}
	}
}

func TestEveryErrorRuleHasRemediation(t *testing.T) {
	t.Parallel()
	for _, r := range rules.Catalog() {
		if r.Severity == engine.Error && strings.TrimSpace(r.Remediation) == "" {
			t.Errorf("%s is an ERROR rule with no remediation text", r.ID)
		}
		if strings.TrimSpace(r.Description) == "" {
			t.Errorf("%s has no description for generated documentation", r.ID)
		}
		if r.Owner == "" || r.Since == "" {
			t.Errorf("%s has no owner or registration date", r.ID)
		}
	}
}

func TestCatalogTotalsMatchDocumentation(t *testing.T) {
	t.Parallel()
	doc := loadCatalog(t)

	// Per-level counts from docs/validation-plane.md §3.8. Asserted explicitly rather than
	// derived, because the totals table is what a reader trusts and it must not be able to
	// drift from the rows above it.
	want := map[int]int{1: 38, 2: 40, 3: 28, 4: 44, 5: 48, 6: 22, 7: 23}
	total := 0
	for level, n := range want {
		total += n
		if got := len(rules.CatalogForLevel(level)); got != n {
			t.Errorf("L%d implements %d rules, catalog §3.8 says %d", level, got, n)
		}
		docCount := 0
		for _, d := range doc {
			if d.level == level {
				docCount++
			}
		}
		if docCount != n {
			t.Errorf("L%d documents %d rows, catalog §3.8 says %d", level, docCount, n)
		}
	}
	if got := rules.Count(); got != total {
		t.Errorf("registry holds %d rules, catalog §3.8 totals %d", got, total)
	}
}

// --- catalog parsing ---------------------------------------------------------------------------

func loadCatalog(t *testing.T) map[engine.RuleID]docRule {
	t.Helper()
	out := map[engine.RuleID]docRule{}
	level := 0
	for _, line := range catalogLines(t) {
		if n, ok := sectionLevel(line); ok {
			level = n
			continue
		}
		row, ok := parseRow(line, level)
		if !ok {
			continue
		}
		out[row.id] = row
	}
	if len(out) == 0 {
		t.Fatalf("parsed no rules out of %s; the parser and the document have diverged", catalogPath)
	}
	return out
}

func catalogLines(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Clean(catalogPath))
	if err != nil {
		t.Fatalf("reading %s: %v", catalogPath, err)
	}
	return strings.Split(string(b), "\n")
}

// sectionLevel recognises the per-level catalog headings, "### 3.1 L1 — …".
func sectionLevel(line string) (int, bool) {
	if !strings.HasPrefix(line, "### 3.") {
		return 0, false
	}
	i := strings.Index(line, " L")
	if i < 0 || i+2 >= len(line) {
		return 0, false
	}
	c := line[i+2]
	if c < '1' || c > '7' {
		return 0, false
	}
	return int(c - '0'), true
}

// ruleIDOfRow extracts the rule ID from a catalog table row, if it is one.
func ruleIDOfRow(line string) (engine.RuleID, bool) {
	if !strings.HasPrefix(strings.TrimSpace(line), "| `L") {
		return "", false
	}
	cells := splitRow(line)
	if len(cells) < 2 {
		return "", false
	}
	id := engine.RuleID(strings.Trim(cells[0], "` "))
	if !id.IsWellFormed() {
		return "", false
	}
	return id, true
}

// parseRow reads one catalog row.
//
// Columns are addressed from the *end* rather than the start, because a couple of Check cells
// legitimately contain a pipe character (`|Σw − 1.0| ≤ 1e−6`), which no amount of naive
// splitting survives. The last four cells are stable across every level's table: severity,
// code, remediation, and either purity (L1–L6) or the database backstop (L7).
func parseRow(line string, level int) (docRule, bool) {
	id, ok := ruleIDOfRow(line)
	if !ok || level == 0 {
		return docRule{}, false
	}
	cells := splitRow(line)
	if len(cells) < 8 {
		return docRule{}, false
	}
	n := len(cells)
	row := docRule{
		id:          id,
		level:       level,
		check:       strings.TrimSpace(strings.Join(cells[3:n-4], " | ")),
		severity:    strings.Trim(cells[n-4], "` "),
		code:        strings.Trim(cells[n-3], "`† "),
		remediation: strings.TrimSpace(cells[n-2]),
		pure:        strings.Trim(cells[n-1], "` "),
	}
	if row.code == "—" {
		row.code = ""
	}
	if level == 7 {
		// The L7 table's final column is the database backstop; every L7 rule is pure.
		row.pure = "Y"
	}
	return row, true
}

// splitRow splits a markdown table row into its cells, dropping the empty leading and trailing
// fragments the outer pipes produce.
func splitRow(line string) []string {
	parts := strings.Split(strings.TrimSpace(line), "|")
	if len(parts) > 0 && strings.TrimSpace(parts[0]) == "" {
		parts = parts[1:]
	}
	if len(parts) > 0 && strings.TrimSpace(parts[len(parts)-1]) == "" {
		parts = parts[:len(parts)-1]
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
