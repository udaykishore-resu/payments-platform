package postgres

import (
	"strconv"
	"strings"
)

// condBuilder accumulates a WHERE clause as predicates plus positional arguments.
//
// It exists so that there is exactly one way to add a dynamic filter in this package, and that
// way cannot produce a statement containing a caller-supplied value. Every method takes the
// value as an argument and emits a `$n` placeholder; there is no method that takes SQL. The
// column names are compile-time string literals from this package.
//
// The alternative — building `WHERE ` + strings.Join(parts, " AND ") where parts are formatted
// with fmt.Sprintf — reads identically at the call site and is a SQL injection sink the first
// time someone passes a value instead of a column name. Making the safe thing the only thing
// available is cheaper than remembering.
type condBuilder struct {
	preds []string
	args  []any
}

// newCond starts a builder whose first arguments are already bound.
//
// Repositories seed it with the tenant, so the tenant is always $1 and every predicate that
// follows numbers from $2. That ordering is not cosmetic: the tenant-leading composite indexes
// in this schema are only usable when tenant_id is an equality predicate, and putting it first
// at the builder level makes it impossible to omit.
func newCond(seed ...any) *condBuilder {
	return &condBuilder{args: append([]any(nil), seed...)}
}

// next returns the placeholder for the argument about to be appended.
func (c *condBuilder) next() string {
	return "$" + strconv.Itoa(len(c.args)+1)
}

// eq appends `col = $n`.
func (c *condBuilder) eq(col string, v any) *condBuilder {
	c.preds = append(c.preds, col+" = "+c.next())
	c.args = append(c.args, v)
	return c
}

// gte appends `col >= $n`.
func (c *condBuilder) gte(col string, v any) *condBuilder {
	c.preds = append(c.preds, col+" >= "+c.next())
	c.args = append(c.args, v)
	return c
}

// lte appends `col <= $n`.
func (c *condBuilder) lte(col string, v any) *condBuilder {
	c.preds = append(c.preds, col+" <= "+c.next())
	c.args = append(c.args, v)
	return c
}

// inStrings appends `col = ANY($n)`.
//
// `= ANY(array)` rather than an expanded `IN ($1,$2,$3)` on purpose. An expanded list produces a
// different statement text for every distinct list length, so a filter over two states and a
// filter over three are two different prepared statements — and a caller that varies the list
// freely fills the plan cache with near-duplicates. `= ANY` is one statement whatever the length.
func (c *condBuilder) inStrings(col string, vs []string) *condBuilder {
	if len(vs) == 0 {
		return c
	}
	c.preds = append(c.preds, col+" = ANY("+c.next()+")")
	c.args = append(c.args, vs)
	return c
}

// ilike appends a case-insensitive prefix match, with the caller's value escaped.
//
// The escaping matters more than it looks. Without it, a search string of `%` matches every row
// in the tenant and turns a search box into a full-table scan; `_` is the same problem one row
// at a time. The value still travels as a bind parameter, so this is not an injection concern —
// it is a "one tenant can make the database do arbitrary work" concern, which is the more likely
// of the two to actually happen.
func (c *condBuilder) ilike(col, v string) *condBuilder {
	if v == "" {
		return c
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
	c.preds = append(c.preds, col+" ILIKE "+c.next()+` ESCAPE '\'`)
	c.args = append(c.args, esc+"%")
	return c
}

// keysetBefore appends the descending keyset predicate `(tcol, icol) < ($n, $n+1)`.
//
// The row-value comparison is the point. Written out as
// `tcol < $1 OR (tcol = $1 AND icol < $2)` it is logically identical and the planner will not
// use the composite index for it — it becomes a filter over the whole tenant's rows instead of a
// range scan. The tuple form matches the index's own ordering and starts the scan exactly at the
// cursor.
func (c *condBuilder) keysetBefore(timeCol, idCol string, cur Cursor) *condBuilder {
	if cur.IsZero() {
		return c
	}
	t := c.next()
	c.args = append(c.args, cur.Time)
	i := c.next()
	c.args = append(c.args, cur.ID)
	c.preds = append(c.preds, "("+timeCol+", "+idCol+") < ("+t+", "+i+")")
	return c
}

// raw appends a predicate that contains no caller data at all.
//
// Every call site passes a compile-time constant. It exists for predicates like
// `outcome = 'SUCCESS'` that are part of the query's meaning rather than part of its input; a
// bind parameter there would defeat the partial indexes, which are matched against literal
// predicates.
func (c *condBuilder) raw(pred string) *condBuilder {
	c.preds = append(c.preds, pred)
	return c
}

// where renders the accumulated predicates. It always emits at least the seeded predicates, and
// returns an empty string when there are none, so callers can concatenate unconditionally.
func (c *condBuilder) where() string {
	if len(c.preds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(c.preds, " AND ")
}

// argsWith appends trailing arguments (the LIMIT, typically) and returns the full argument list.
func (c *condBuilder) argsWith(extra ...any) []any {
	return append(append([]any(nil), c.args...), extra...)
}

// limitPlaceholder returns the placeholder for a LIMIT appended after all predicates.
func (c *condBuilder) limitPlaceholder() string { return c.next() }
