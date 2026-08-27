package contract

import (
	"fmt"
	"sort"
	"strings"
)

// BreakingChanges reports every way `cur` breaks a consumer written against `prev`.
//
// The rule this encodes is baseline §13.1: **within a major version, only additive changes to
// optional fields**. A breaking change is not forbidden — it is a new `.vN+1` type published
// alongside the old one until every consumer has migrated. What is forbidden is making it in
// place, because the topic already holds events written under the old shape and a consumer
// subscribed to `.v1` has no way to know the rules changed.
//
// Each rule below is stated as "what breaks", because a compatibility checker whose rules are
// stated as "what is allowed" quietly permits everything nobody thought of.
func BreakingChanges(prev, cur *Schema) []string {
	var out []string
	out = append(out, breakingAt("data", prev, prev.Root, cur, cur.Root)...)
	sort.Strings(out)
	return out
}

func breakingAt(path string, prevDoc *Schema, prev map[string]any, curDoc *Schema, cur map[string]any) []string {
	prev = deref(prevDoc, prev)
	cur = deref(curDoc, cur)

	var out []string
	add := func(format string, args ...any) {
		out = append(out, path+": "+fmt.Sprintf(format, args...))
	}

	// A type change is the most direct break there is: every consumer's decode fails.
	if pt, ct := typeName(prev), typeName(cur); pt != ct {
		add("type changed from %q to %q", pt, ct)
		return out
	}

	prevProps := propsOf(prev)
	curProps := propsOf(cur)
	prevReq := setOf(requiredOf(prev))
	curReq := setOf(requiredOf(cur))

	for _, name := range sortedKeys(prevProps) {
		if _, still := curProps[name]; !still {
			// A removed field breaks every consumer that reads it, and the schema is the only
			// place that ever said the field existed.
			add("property %q was removed", name)
			continue
		}
		if prevReq[name] && !curReq[name] {
			// Demoting a required field to optional is not additive: a consumer written against
			// the old schema is entitled to assume the field is present and will nil-deref the
			// first event that omits it.
			add("property %q was demoted from required to optional", name)
		}
		out = append(out, breakingAt(path+"."+name,
			prevDoc, mapOf(prevProps[name]), curDoc, mapOf(curProps[name]))...)
	}

	for _, name := range sortedKeys(curProps) {
		if _, existed := prevProps[name]; existed {
			continue
		}
		if curReq[name] {
			// A new *required* field is a breaking change for the producer's own history: every
			// event already on the topic lacks it, so a replay against the new schema fails.
			add("new property %q was added as required; a new field must be optional", name)
		}
	}

	// Widening a constrained value is safe; narrowing it rejects instances the old schema
	// accepted, including instances already published.
	out = append(out, narrowings(path, prev, cur)...)

	if prevExtra, curExtra := additionalAllowed(prev), additionalAllowed(cur); prevExtra && !curExtra {
		add("additionalProperties was tightened from true to false")
	}

	// Arrays: the element schema is subject to the same rules.
	if pi, ok := prev["items"].(map[string]any); ok {
		if ci, ok := cur["items"].(map[string]any); ok {
			out = append(out, breakingAt(path+"[]", prevDoc, pi, curDoc, ci)...)
		}
	}
	return out
}

// narrowings reports constraint changes that reject previously valid instances.
func narrowings(path string, prev, cur map[string]any) []string {
	var out []string
	add := func(format string, args ...any) {
		out = append(out, path+": "+fmt.Sprintf(format, args...))
	}

	if p, ok := prev["pattern"].(string); ok {
		c, present := cur["pattern"].(string)
		switch {
		case !present:
			// Dropping a pattern only widens the accepted set, which is safe.
		case c != p:
			// Proving one regular expression's language contains another's is decidable but not
			// cheap, and a checker that guessed would be worse than one that refuses: any pattern
			// change is reported, and a genuine widening is acknowledged in the review that
			// changes it.
			add("pattern changed from %q to %q; a pattern change cannot be proven non-narrowing", p, c)
		}
	} else if c, present := cur["pattern"].(string); present {
		add("a pattern %q was added where there was none", c)
	}

	if pe, ok := prev["enum"].([]any); ok {
		ce, present := cur["enum"].([]any)
		if !present {
			// Removing the enum widens the accepted set. Safe for the schema; the consumer
			// contract test is what catches a consumer that switched exhaustively.
			return out
		}
		curSet := map[string]bool{}
		for _, v := range ce {
			curSet[fmt.Sprint(v)] = true
		}
		for _, v := range pe {
			if !curSet[fmt.Sprint(v)] {
				// A value already published is now illegal: replaying the archive fails.
				add("enum value %v was removed", v)
			}
		}
	} else if _, present := cur["enum"].([]any); present {
		add("an enum was added where the field was previously unconstrained")
	}

	type bound struct {
		key      string
		narrower func(prev, cur float64) bool
		how      string
	}
	for _, b := range []bound{
		{"minLength", func(p, c float64) bool { return c > p }, "raised"},
		{"maxLength", func(p, c float64) bool { return c < p }, "lowered"},
		{"minimum", func(p, c float64) bool { return c > p }, "raised"},
		{"maximum", func(p, c float64) bool { return c < p }, "lowered"},
		{"minItems", func(p, c float64) bool { return c > p }, "raised"},
		{"maxItems", func(p, c float64) bool { return c < p }, "lowered"},
	} {
		pv, hasPrev := numberOf(prev[b.key])
		cv, hasCur := numberOf(cur[b.key])
		switch {
		case hasPrev && hasCur && b.narrower(pv, cv):
			add("%s was %s from %v to %v", b.key, b.how, pv, cv)
		case !hasPrev && hasCur:
			add("%s %v was added where the field was previously unbounded", b.key, cv)
		}
	}
	return out
}

// deref resolves a node that is a bare local $ref, so the comparison walks the same shapes on
// both sides even when one version inlined what the other factored into $defs.
func deref(doc *Schema, node map[string]any) map[string]any {
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	target, err := doc.resolve(ref)
	if err != nil {
		return node
	}
	return target
}

func typeName(node map[string]any) string {
	if t, ok := node["type"].(string); ok {
		return t
	}
	// An absent `type` means "any", which is a real and distinct answer: a version that adds a
	// type where there was none has narrowed the field.
	return ""
}

func propsOf(node map[string]any) map[string]any {
	if p, ok := node["properties"].(map[string]any); ok {
		return p
	}
	return map[string]any{}
}

func requiredOf(node map[string]any) []string {
	raw, ok := node["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func additionalAllowed(node map[string]any) bool {
	if b, ok := node["additionalProperties"].(bool); ok {
		return b
	}
	return true
}

func mapOf(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func setOf(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SplitVersion separates an event type into its base name and major version:
// "payment.captured.v1" -> ("payment.captured", 1).
func SplitVersion(eventType string) (base string, major int, ok bool) {
	i := strings.LastIndex(eventType, ".v")
	if i < 0 {
		return "", 0, false
	}
	base = eventType[:i]
	digits := eventType[i+2:]
	if digits == "" {
		return "", 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return "", 0, false
		}
		major = major*10 + int(r-'0')
	}
	return base, major, true
}
