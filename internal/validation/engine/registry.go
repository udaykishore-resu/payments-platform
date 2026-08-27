package engine

import (
	"fmt"
	"sort"
	"sync"
)

// Status is a registry entry's lifecycle.
type Status string

const (
	// StatusActive means the rule is live: it is in a rule set and it evaluates.
	StatusActive Status = "ACTIVE"

	// StatusRetired means the rule no longer evaluates but its entry survives, because audit
	// records written while it was live still reference its ID and a reader of those records
	// must be able to find out what the ID meant. This is also why an ID is never reused.
	StatusRetired Status = "RETIRED"
)

// Registration is everything the platform knows about a rule without running it.
//
// The registry is the reason a question like "what does this platform actually check?" has an
// answer that can be computed rather than researched. CI counts it, documentation generation
// renders it, the catalog test diffs it against docs/validation-plane.md, and a support
// engineer holding a rule ID from a customer's error response reads the remediation out of it.
type Registration struct {
	// ID is the stable identifier. It carries its own level.
	ID RuleID

	// Severity is the declared severity, independent of the stage the rule currently runs at.
	Severity Severity

	// Code is the apierror code a failure of this rule maps to. Empty for warning-only rules
	// that have no catalog code, which the catalog documents with an em dash.
	Code string

	// Description is the one-line statement of what the rule asserts, for generated docs.
	Description string

	// Remediation is the caller-facing sentence explaining how to make the rule pass. It is
	// mandatory for Error rules: a rejection a merchant's engineer cannot act on is a support
	// ticket the platform chose to receive.
	Remediation string

	// Pure records whether the rule is total, deterministic and free of network, clock and
	// shared-counter reads. It is a declaration, cross-checked by tests rather than inferred,
	// because the property that matters — "no impure rule is on the payment hot path" — has to
	// be assertable at build time.
	Pure bool

	// Stage is the promotion stage the rule ships at. New rules register as Shadow.
	Stage Stage

	// Owner is the team accountable for the rule, from the PR that introduced it.
	Owner string

	// Since is the ISO date the rule was registered, which is what makes "this rejection did
	// not exist last quarter" answerable.
	Since string

	// Status distinguishes a live rule from a retired one.
	Status Status
}

// RuleRegistry is the process-wide catalog of rule metadata.
//
// It is a type with a package-level instance rather than a bare map so that `Registry.All()`
// reads as what it is, and so that a test can construct an isolated registry when it needs to
// assert registration behaviour without touching the real one.
type RuleRegistry struct {
	mu      sync.RWMutex
	entries map[RuleID]Registration
}

// Registry is the process-wide registry every rules package registers into at init.
var Registry = NewRegistry()

// NewRegistry returns an empty registry. Production code uses the package-level Registry;
// this exists for tests of the registry itself.
func NewRegistry() *RuleRegistry {
	return &RuleRegistry{entries: map[RuleID]Registration{}}
}

// Register records a rule's metadata.
//
// It panics on a duplicate ID, on a malformed ID, and on an Error rule with no remediation.
// Panicking is correct here and only here: these are programming errors detectable at init,
// which means the first test run catches them and nothing reaches a request path (conventions
// rule 9). The alternative — returning an error from init-time registration — would be
// silently ignored at exactly the call sites that need to fail loudly.
func (reg *RuleRegistry) Register(r Registration) {
	if !r.ID.IsWellFormed() {
		panic(fmt.Sprintf("validation: malformed rule ID %q: want ^L[1-7]\\.[A-Z0-9_]{4,60}$", r.ID))
	}
	if r.Severity == Error && r.Remediation == "" {
		panic(fmt.Sprintf("validation: rule %s is an ERROR with no remediation text", r.ID))
	}
	if r.Status == "" {
		r.Status = StatusActive
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if existing, ok := reg.entries[r.ID]; ok {
		panic(fmt.Sprintf("validation: duplicate rule ID %s (already registered as %q)",
			r.ID, existing.Description))
	}
	reg.entries[r.ID] = r
}

// All returns every registration, sorted by ID.
//
// Sorted rather than in registration order because registration order is package-init order,
// which is not stable across builds, and a documentation generator whose output reorders
// itself between runs produces a diff nobody can review.
func (reg *RuleRegistry) All() []Registration {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	out := make([]Registration, 0, len(reg.entries))
	for _, r := range reg.entries {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ForLevel returns the registrations for one level, sorted by ID.
func (reg *RuleRegistry) ForLevel(level int) []Registration {
	var out []Registration
	for _, r := range reg.All() {
		if r.ID.Level() == level {
			out = append(out, r)
		}
	}
	return out
}

// Lookup returns the registration for id.
func (reg *RuleRegistry) Lookup(id RuleID) (Registration, bool) {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.entries[id]
	return r, ok
}

// StageOf returns the registered stage for id. An unregistered ID answers Shadow: a rule the
// registry has never heard of must not be able to reject anything, because the registry is
// what documentation, metrics and audit all key on, and an unregistered rejection is
// unexplainable to the person who receives it.
func (reg *RuleRegistry) StageOf(id RuleID) Stage {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	r, ok := reg.entries[id]
	if !ok {
		return Shadow
	}
	return r.Stage
}

// Count returns the number of registered rules. Used by the CI check that the documented total
// and the implemented total agree.
func (reg *RuleRegistry) Count() int {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return len(reg.entries)
}

// Register records a rule in the process-wide registry. Called from each rules package's init.
func Register(r Registration) { Registry.Register(r) }

// MustRegisterAll registers every entry, in order. A convenience for a rules package that
// declares its catalog as a table.
func MustRegisterAll(rs ...Registration) {
	for _, r := range rs {
		Registry.Register(r)
	}
}

// Lookup returns the process-wide registration for id.
func Lookup(id RuleID) (Registration, bool) { return Registry.Lookup(id) }

// StageOf returns the process-wide registered stage for id.
func StageOf(id RuleID) Stage { return Registry.StageOf(id) }

// IsRegisteredPure reports whether id is registered and declared pure.
func IsRegisteredPure(id RuleID) bool {
	r, ok := Lookup(id)
	return ok && r.Pure
}
