// Package contract holds the consumer-driven contract suite for the platform's published event
// schemas.
//
// It is deliberately **untagged**: it needs no database, no broker and no running service, so it
// runs in the cheapest CI stage and on a laptop with `go test ./tests/...`. A contract suite that
// only runs when infrastructure is available is a contract suite that stops running.
//
// What it asserts, and why each half is necessary:
//
//   - Producer conformance: every registered event type, encoded through the *real* codec
//     (internal/events), yields an envelope and a payload that satisfy the JSON Schema published
//     in api/events/. Without this, a schema is a document rather than a contract.
//   - Consumer satisfaction: each declared consumer's required field set is still provided.
//     Schema conformance alone does not give you this — a producer may legally stop populating an
//     *optional* field, and every consumer that quietly depended on it breaks in production while
//     CI stays green.
//   - Schema compatibility: within a major version, only additive changes to optional fields are
//     permitted (baseline §13.1). A major bump must leave the previous major's schema in place for
//     the dual-publish window.
package contract

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaDir is api/events relative to this package.
const SchemaDir = "../../api/events"

// Schema is a parsed JSON Schema document.
//
// # Why this validator is hand-written
//
// It implements exactly the constructs the schemas in api/events/ use — `type`, `required`,
// `properties`, `enum`, `additionalProperties`, plus `const`, `pattern`, `minLength`/`maxLength`,
// `minimum`/`maximum`, `items`, `minItems`/`maxItems`, local `$ref` into `$defs`, and the
// `date-time`/`date` formats. Nothing else. A full JSON Schema 2020-12 implementation would be a
// new module in a go.mod that is pinned and shared, for a check that fits in three hundred lines.
//
// The trade-off is explicit and has a tripwire: `UnsupportedKeywords` reports any keyword a schema
// uses that this validator does not understand, and TestValidatorUnderstandsEverySchemaKeyword
// fails the build when one appears. That converts "the validator silently ignored the new
// construct" — the failure mode that makes a hand-rolled validator dangerous — into a build
// failure that names the keyword and the file. If that tripwire ever fires for a construct that is
// genuinely needed, the right answer is to reconsider the dependency, not to widen the allowlist.
type Schema struct {
	// Name is the file name, used in failure messages.
	Name string
	// Root is the decoded document.
	Root map[string]any
}

// LoadSchema reads and parses one schema file.
func LoadSchema(dir, name string) (*Schema, error) {
	b, err := os.ReadFile(dir + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	return &Schema{Name: name, Root: root}, nil
}

// Title returns the schema's declared title, or its file name.
func (s *Schema) Title() string {
	if t, ok := s.Root["title"].(string); ok && t != "" {
		return t
	}
	return s.Name
}

// Examples returns the schema's declared example instances.
//
// They matter beyond documentation: the producer test encodes each one through the real codec, so
// an example that drifts from its own schema is a build failure rather than a misleading snippet
// somebody copies into a consumer.
func (s *Schema) Examples() []map[string]any {
	raw, ok := s.Root["examples"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// StringSlice returns a top-level array-of-strings annotation such as x-consumers.
func (s *Schema) StringSlice(key string) []string {
	raw, ok := s.Root[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if str, ok := v.(string); ok {
			out = append(out, str)
		}
	}
	return out
}

// Validate reports every way doc violates the schema. An empty result means conforming.
//
// Every problem is reported rather than the first: a producer change that breaks four fields is
// one fix, and reporting one field per run turns it into four.
func (s *Schema) Validate(doc any) []string {
	return s.node("data", s.Root, doc)
}

// supportedKeywords is the set this validator implements or knowingly ignores.
//
// The annotations (`x-*`), the documentation keywords (`title`, `description`, `examples`) and the
// identity keywords (`$id`, `$schema`) are ignored by design. Everything else in this map is
// enforced below.
var supportedKeywords = map[string]bool{
	"$schema": true, "$id": true, "$ref": true, "$defs": true,
	"title": true, "description": true, "examples": true,
	"type": true, "required": true, "properties": true, "additionalProperties": true,
	"enum": true, "const": true, "pattern": true, "format": true,
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"items": true, "minItems": true, "maxItems": true,
}

// UnsupportedKeywords returns every keyword the schema uses that this validator does not
// implement, sorted. Empty means the schema is fully covered.
func (s *Schema) UnsupportedKeywords() []string {
	seen := map[string]bool{}
	var walk func(n any)
	walk = func(n any) {
		switch v := n.(type) {
		case map[string]any:
			for k, child := range v {
				if strings.HasPrefix(k, "x-") {
					continue
				}
				if !supportedKeywords[k] {
					seen[k] = true
					continue
				}
				switch k {
				case "properties", "$defs":
					if m, ok := child.(map[string]any); ok {
						for _, sub := range m {
							walk(sub)
						}
					}
				case "items", "additionalProperties":
					walk(child)
				case "required", "enum", "const", "examples", "type", "pattern", "format":
					// Terminal values; their contents are data, not schemas.
				default:
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(s.Root)
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// resolve follows a local `#/$defs/<name>` reference. Remote references are refused rather than
// fetched: a validator that reaches the network to decide whether an event is legal is a validator
// that fails differently on a CI runner than on a laptop.
func (s *Schema) resolve(ref string) (map[string]any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q: only local #/$defs/<name> references are resolvable", ref)
	}
	defs, ok := s.Root["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q but the schema declares no $defs", ref)
	}
	target, ok := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("$ref %q does not resolve", ref)
	}
	return target, nil
}

// node validates v against one schema node at the given document path.
func (s *Schema) node(path string, schema map[string]any, v any) []string {
	if ref, ok := schema["$ref"].(string); ok {
		target, err := s.resolve(ref)
		if err != nil {
			return []string{path + ": " + err.Error()}
		}
		schema = target
	}

	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}

	if want, ok := schema["type"]; ok && !typeMatches(want, v) {
		add("is %s, want type %v", kindOf(v), want)
		// Every remaining keyword is about a value of the declared type, so continuing would
		// produce a cascade of consequential noise around one real problem.
		return problems
	}
	if c, ok := schema["const"]; ok && !reflect.DeepEqual(normalize(c), normalize(v)) {
		add("is %v, want the const %v", v, c)
	}
	if raw, ok := schema["enum"].([]any); ok {
		match := false
		for _, allowed := range raw {
			if reflect.DeepEqual(normalize(allowed), normalize(v)) {
				match = true
				break
			}
		}
		if !match {
			add("is %v, which is not in the enum %v", v, raw)
		}
	}

	switch typed := v.(type) {
	case string:
		problems = append(problems, s.checkString(path, schema, typed)...)
	case float64:
		if lo, ok := numberOf(schema["minimum"]); ok && typed < lo {
			add("is %v, below minimum %v", typed, lo)
		}
		if hi, ok := numberOf(schema["maximum"]); ok && typed > hi {
			add("is %v, above maximum %v", typed, hi)
		}
	case []any:
		problems = append(problems, s.checkArray(path, schema, typed)...)
	case map[string]any:
		problems = append(problems, s.checkObject(path, schema, typed)...)
	}
	return problems
}

func (s *Schema) checkString(path string, schema map[string]any, v string) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}
	if p, ok := schema["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		switch {
		case err != nil:
			add("schema pattern %q does not compile in Go's regexp: %v", p, err)
		case !re.MatchString(v):
			add("%q does not match %s", v, p)
		}
	}
	if n, ok := numberOf(schema["minLength"]); ok && float64(len(v)) < n {
		add("is %d characters, below minLength %v", len(v), n)
	}
	if n, ok := numberOf(schema["maxLength"]); ok && float64(len(v)) > n {
		add("is %d characters, above maxLength %v", len(v), n)
	}
	// Only the two temporal formats are enforced. `uri` is left unchecked on purpose: Go's
	// url.Parse accepts almost anything, so a check would assert nothing while reading as though
	// it did.
	switch schema["format"] {
	case "date-time":
		if _, err := time.Parse(time.RFC3339, v); err != nil {
			add("%q is not an RFC 3339 date-time", v)
		}
	case "date":
		if _, err := time.Parse("2006-01-02", v); err != nil {
			add("%q is not an RFC 3339 full-date", v)
		}
	}
	return problems
}

func (s *Schema) checkArray(path string, schema map[string]any, v []any) []string {
	var problems []string
	if n, ok := numberOf(schema["minItems"]); ok && float64(len(v)) < n {
		problems = append(problems, fmt.Sprintf("%s: has %d items, below minItems %v", path, len(v), n))
	}
	if n, ok := numberOf(schema["maxItems"]); ok && float64(len(v)) > n {
		problems = append(problems, fmt.Sprintf("%s: has %d items, above maxItems %v", path, len(v), n))
	}
	if items, ok := schema["items"].(map[string]any); ok {
		for i, elem := range v {
			problems = append(problems, s.node(fmt.Sprintf("%s[%d]", path, i), items, elem)...)
		}
	}
	return problems
}

func (s *Schema) checkObject(path string, schema map[string]any, v map[string]any) []string {
	var problems []string
	props, _ := schema["properties"].(map[string]any)

	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := v[name]; !present {
				problems = append(problems, fmt.Sprintf("%s.%s: required field is missing", path, name))
			}
		}
	}
	// additionalProperties:false is the keyword that makes a schema a contract rather than a
	// suggestion: without it, a consumer cannot tell a typo from a new field.
	if extra, ok := schema["additionalProperties"].(bool); ok && !extra {
		names := make([]string, 0, len(v))
		for k := range v {
			if _, declared := props[k]; !declared {
				names = append(names, k)
			}
		}
		sort.Strings(names)
		for _, k := range names {
			problems = append(problems, fmt.Sprintf("%s.%s: undeclared field (additionalProperties is false)", path, k))
		}
	}
	// Sorted so a multi-field failure reads the same on every run; map order would make two runs
	// of the same failure produce two different diffs.
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		child, present := v[name]
		if !present {
			continue
		}
		sub, ok := props[name].(map[string]any)
		if !ok {
			continue
		}
		problems = append(problems, s.node(path+"."+name, sub, child)...)
	}
	return problems
}

// typeMatches implements JSON Schema's type keyword over encoding/json's decoded shapes.
//
// The integer case is the one that matters here: every monetary amount in this platform is an
// integer of minor units, JSON has only one number type, and encoding/json decodes all of them to
// float64. Checking that the float64 holds an integral value is what stops `{"amount": 10.5}` from
// being accepted as money.
func typeMatches(want any, v any) bool {
	switch t := want.(type) {
	case string:
		return typeNameMatches(t, v)
	case []any:
		for _, alt := range t {
			if name, ok := alt.(string); ok && typeNameMatches(name, v) {
				return true
			}
		}
		return false
	default:
		return true
	}
}

func typeNameMatches(name string, v any) bool {
	switch name {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "null":
		return v == nil
	default:
		return true
	}
}

func kindOf(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case float64:
		if t == float64(int64(t)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

func numberOf(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// normalize makes an int from a Go literal comparable with a float64 from encoding/json, so a
// const or enum written in Go and one read from a schema file compare equal.
func normalize(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	default:
		return v
	}
}
