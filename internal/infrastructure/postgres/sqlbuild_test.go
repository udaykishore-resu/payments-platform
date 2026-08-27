package postgres

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestCondBuilderRendersEveryFilterCombination walks the power set of the payment filter's
// predicates and asserts, for each, that the rendered SQL and the argument list agree.
//
// The property that matters is not "the SQL looks right" — it is that **every caller-supplied
// value appears in the argument list and none appears in the statement text**, for every
// combination. A builder that is safe for eight of nine combinations is not safe.
func TestCondBuilderRendersEveryFilterCombination(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC)

	// Each option appends one predicate and zero or more arguments.
	type option struct {
		name  string
		apply func(*condBuilder)
		// wantPred is the predicate fragment, with $n elided — placeholders depend on which
		// other options are active, and asserting them literally would make this test a
		// restatement of the implementation rather than a check on it.
		wantPred string
		wantArgs []any
	}

	options := []option{
		{
			name:     "merchant",
			apply:    func(c *condBuilder) { c.eq("p.merchant_id", "mrc_1") },
			wantPred: "p.merchant_id = ",
			wantArgs: []any{"mrc_1"},
		},
		{
			name:     "states",
			apply:    func(c *condBuilder) { c.inStrings("p.state", []string{"CREATED", "PROCESSING"}) },
			wantPred: "p.state = ANY(",
			wantArgs: []any{[]string{"CREATED", "PROCESSING"}},
		},
		{
			name:     "currency",
			apply:    func(c *condBuilder) { c.eq("p.currency", "EUR") },
			wantPred: "p.currency = ",
			wantArgs: []any{"EUR"},
		},
		{
			name:     "gateway",
			apply:    func(c *condBuilder) { c.eq("p.selected_gateway", "stripe") },
			wantPred: "p.selected_gateway = ",
			wantArgs: []any{"stripe"},
		},
		{
			name:     "created after",
			apply:    func(c *condBuilder) { c.gte("p.created_at", now) },
			wantPred: "p.created_at >= ",
			wantArgs: []any{now},
		},
		{
			name:     "created before",
			apply:    func(c *condBuilder) { c.lte("p.created_at", now) },
			wantPred: "p.created_at <= ",
			wantArgs: []any{now},
		},
		{
			name:     "amount min",
			apply:    func(c *condBuilder) { c.gte("p.amount", int64(100)) },
			wantPred: "p.amount >= ",
			wantArgs: []any{int64(100)},
		},
		{
			name:     "amount max",
			apply:    func(c *condBuilder) { c.lte("p.amount", int64(9999)) },
			wantPred: "p.amount <= ",
			wantArgs: []any{int64(9999)},
		},
		{
			name:     "keyset cursor",
			apply:    func(c *condBuilder) { c.keysetBefore("p.created_at", "p.payment_id", Cursor{Time: now, ID: "pay_x"}) },
			wantPred: "(p.created_at, p.payment_id) < (",
			wantArgs: []any{now, "pay_x"},
		},
	}

	// 2^9 = 512 combinations. Exhaustive rather than sampled: the bug this guards against is a
	// placeholder that is correct in isolation and wrong once another option shifts the numbering.
	for mask := 0; mask < 1<<len(options); mask++ {

		names := make([]string, 0, len(options))
		for i, o := range options {
			if mask&(1<<i) != 0 {
				names = append(names, o.name)
			}
		}
		label := strings.Join(names, "+")
		if label == "" {
			label = "none"
		}

		t.Run(label, func(t *testing.T) {
			t.Parallel()

			c := newCond("ten_1")
			c.raw("p.tenant_id = $1")
			wantArgs := []any{"ten_1"}
			var wantPreds []string
			wantPreds = append(wantPreds, "p.tenant_id = $1")

			for i, o := range options {
				if mask&(1<<i) == 0 {
					continue
				}
				o.apply(c)
				wantPreds = append(wantPreds, o.wantPred)
				wantArgs = append(wantArgs, o.wantArgs...)
			}

			where := c.where()
			if !strings.HasPrefix(where, " WHERE ") {
				t.Fatalf("where() = %q, want a leading WHERE", where)
			}
			for _, p := range wantPreds {
				if !strings.Contains(where, p) {
					t.Fatalf("missing predicate %q in %q", p, where)
				}
			}
			if got, want := strings.Count(where, " AND "), len(wantPreds)-1; got != want {
				t.Fatalf("%d AND separators for %d predicates: %q", got, len(wantPreds), where)
			}

			// Placeholders must be dense and one-based, so that appending the LIMIT lands on the
			// next free number. A gap or a repeat here is a mis-bound argument at runtime.
			assertPlaceholdersAreDense(t, where, len(wantArgs))

			args := c.argsWith(50)
			if len(args) != len(wantArgs)+1 {
				t.Fatalf("argsWith produced %d args, want %d", len(args), len(wantArgs)+1)
			}
			for i := range wantArgs {
				if !reflect.DeepEqual(args[i], wantArgs[i]) {
					t.Fatalf("arg %d = %#v, want %#v", i, args[i], wantArgs[i])
				}
			}
			if args[len(args)-1] != 50 {
				t.Fatalf("the LIMIT must be the last argument, got %#v", args[len(args)-1])
			}
			if want := "$" + strconv.Itoa(len(wantArgs)+1); c.limitPlaceholder() != want {
				t.Fatalf("limitPlaceholder = %s, want %s", c.limitPlaceholder(), want)
			}

			// The load-bearing assertion: no caller value appears in the statement text.
			for _, a := range wantArgs {
				if s, ok := a.(string); ok && s != "ten_1" && strings.Contains(where, s) {
					t.Fatalf("value %q was interpolated into the SQL: %q", s, where)
				}
			}
		})
	}
}

func assertPlaceholdersAreDense(t *testing.T, sql string, want int) {
	t.Helper()
	seen := map[int]bool{}
	for i := 0; i < len(sql); i++ {
		if sql[i] != '$' {
			continue
		}
		j := i + 1
		for j < len(sql) && sql[j] >= '0' && sql[j] <= '9' {
			j++
		}
		n, err := strconv.Atoi(sql[i+1 : j])
		if err != nil {
			t.Fatalf("malformed placeholder at %d in %q", i, sql)
		}
		if seen[n] {
			t.Fatalf("placeholder $%d appears twice in %q", n, sql)
		}
		seen[n] = true
	}
	for n := 1; n <= want; n++ {
		if !seen[n] {
			t.Fatalf("placeholder $%d is missing from %q (expected 1..%d)", n, sql, want)
		}
	}
	if len(seen) != want {
		t.Fatalf("%d distinct placeholders, want %d, in %q", len(seen), want, sql)
	}
}

// TestEmptyFiltersAppendNothing proves that an absent filter costs nothing — no predicate, no
// argument, no shifted placeholder for the filters that follow it.
func TestEmptyFiltersAppendNothing(t *testing.T) {
	t.Parallel()

	c := newCond("ten_1")
	c.inStrings("p.state", nil)
	c.inStrings("p.state", []string{})
	c.ilike("m.display_name", "")
	c.keysetBefore("p.created_at", "p.payment_id", Cursor{})

	if got := c.where(); got != "" {
		t.Fatalf("where() = %q, want empty for a builder with no predicates", got)
	}
	if got := len(c.argsWith()); got != 1 {
		t.Fatalf("%d args, want only the seeded tenant", got)
	}
}

// TestILikeEscapesMetacharacters guards a denial-of-service, not an injection. The value already
// travels as a bind parameter; an unescaped `%` would turn a merchant search into a full scan of
// the tenant's merchants, which one tenant can do to every other tenant on the cluster.
func TestILikeEscapesMetacharacters(t *testing.T) {
	t.Parallel()

	cases := []struct{ in, want string }{
		{"acme", "acme%"},
		{"%", `\%%`},
		{"_", `\_%`},
		{`a\b`, `a\\b%`},
		{"50%_off", `50\%\_off%`},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			c := newCond()
			c.ilike("m.display_name", tc.in)
			args := c.argsWith()
			if len(args) != 1 {
				t.Fatalf("%d args, want 1", len(args))
			}
			if got := args[0].(string); got != tc.want {
				t.Fatalf("ilike(%q) bound %q, want %q", tc.in, got, tc.want)
			}
			if !strings.Contains(c.where(), `ESCAPE '\'`) {
				t.Fatalf("the ESCAPE clause is required for the backslash escaping to apply: %q",
					c.where())
			}
		})
	}
}

// TestKeysetUsesRowValueComparison is a performance property with a correctness-shaped failure
// mode: the OR-expanded form is logically identical and the planner will not use the composite
// index for it, which turns cursor pagination into a filter over every row in the tenant.
func TestKeysetUsesRowValueComparison(t *testing.T) {
	t.Parallel()
	c := newCond()
	c.keysetBefore("p.created_at", "p.payment_id", Cursor{Time: time.Unix(1, 0), ID: "pay_x"})
	where := c.where()
	if !strings.Contains(where, "(p.created_at, p.payment_id) < ($1, $2)") {
		t.Fatalf("keyset predicate must be a row-value comparison, got %q", where)
	}
	if strings.Contains(where, " OR ") {
		t.Fatalf("the OR-expanded keyset form defeats the index: %q", where)
	}
}
