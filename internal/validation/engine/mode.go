package engine

import (
	"context"
	"sort"
	"time"
)

// Stage is a rule's position on the shadow → warn → enforce promotion path.
//
// Why a new rule may not simply start rejecting. A rule that looks obviously correct — "the
// amount must be at least the method minimum" — will reject some fraction of a merchant's
// traffic that has been succeeding for two years, because the real world contains a legacy
// integration nobody documented. Shipping it straight to Enforce turns a correctness
// improvement into a revenue incident, discovered from the merchant's side. So a rule ships
// evaluating but silent, its would-reject rate is measured against production traffic, and it
// is promoted only when someone has looked at what it would have rejected.
//
// The stage is registry state, not a code edit, so demoting Enforce → Warn during an incident
// is a configuration publish propagating in seconds rather than a deploy. That is the lever,
// and a lever that requires a release is not a lever. See docs/validation-plane.md §4.3.
type Stage uint8

const (
	// Shadow evaluates the rule on every request and records the outcome to metrics and the
	// audit record, but the report drops the outcome before anything can act on it. The rule
	// cannot fail a request, cannot appear in `details[]`, and cannot short-circuit a set.
	Shadow Stage = iota

	// Warn evaluates the rule and surfaces the outcome at WARNING severity: it appears in
	// `details[]` and on the merchant's dashboard, and the operation still succeeds.
	Warn

	// Enforce evaluates the rule at its declared severity. This is where a rule spends its
	// working life.
	Enforce
)

// String satisfies fmt.Stringer. The rendered form is a metric label value, so it is part of
// the published contract.
func (s Stage) String() string {
	switch s {
	case Shadow:
		return "shadow"
	case Warn:
		return "warn"
	default:
		return "enforce"
	}
}

// ParseStage converts the metric-label form back to a Stage. Unknown input returns Shadow and
// false: an unrecognized stage in configuration must fail *closed with respect to enforcement*
// — the safe reading of "I do not understand this setting" is "do not let this rule reject
// anything", because the alternative is a typo that starts rejecting production traffic.
func ParseStage(s string) (Stage, bool) {
	switch s {
	case "shadow":
		return Shadow, true
	case "warn":
		return Warn, true
	case "enforce":
		return Enforce, true
	}
	return Shadow, false
}

// StageLookup resolves the stage a rule runs at for one evaluation.
//
// It is an interface with a single method, and it takes no tenant or clock, because the
// RuleSet must stay pure: whoever binds a tenant and a time does it once, outside evaluation,
// and hands the set the resulting flat lookup.
type StageLookup interface {
	StageFor(id RuleID) Stage
}

// StageFunc adapts a function to StageLookup.
type StageFunc func(id RuleID) Stage

// StageFor satisfies StageLookup.
func (f StageFunc) StageFor(id RuleID) Stage { return f(id) }

// RegistryStages is the default lookup: every rule runs at the stage it was registered with.
var RegistryStages StageLookup = StageFunc(StageOf)

// StageOverride is a time-boxed deviation from a rule's registered stage.
//
// ExpiresAt is mandatory in practice: an override without an expiry is a permanent second
// definition of the rule that nobody remembers agreeing to, which is why the platform's CI
// check TestNoPermanentOverrides rejects a zero expiry. It is not enforced here because this
// type is also how an incident responder writes a one-hour demotion, and refusing to construct
// one at 03:00 is the wrong failure mode.
type StageOverride struct {
	Stage     Stage
	ExpiresAt time.Time
	Reason    string
}

// Stages resolves the effective stage for a rule, applying per-tenant overrides on top of the
// registered stage.
//
// A rule is commonly Enforce platform-wide and Warn for one named tenant during their
// migration window. Modelling that as an override rather than as a second registration keeps
// exactly one definition of the rule and makes the deviation enumerable — you can ask "which
// rules is tenant X not subject to today", which is a question compliance eventually asks.
type Stages struct {
	base      StageLookup
	global    map[RuleID]StageOverride
	perTenant map[string]map[RuleID]StageOverride
}

// NewStages returns a resolver over base. A nil base falls back to the registered stages.
func NewStages(base StageLookup) *Stages {
	if base == nil {
		base = RegistryStages
	}
	return &Stages{
		base:      base,
		global:    map[RuleID]StageOverride{},
		perTenant: map[string]map[RuleID]StageOverride{},
	}
}

// Override records a platform-wide deviation. It returns the receiver so overrides chain in a
// configuration loader.
func (s *Stages) Override(id RuleID, o StageOverride) *Stages {
	s.global[id] = o
	return s
}

// OverrideForTenant records a deviation for one tenant only.
func (s *Stages) OverrideForTenant(tenant string, id RuleID, o StageOverride) *Stages {
	m := s.perTenant[tenant]
	if m == nil {
		m = map[RuleID]StageOverride{}
		s.perTenant[tenant] = m
	}
	m[id] = o
	return s
}

// Resolve returns the effective stage for id, for tenant, at now. The most specific unexpired
// override wins; an expired override is ignored, which is what makes a migration window close
// by itself rather than by someone remembering.
func (s *Stages) Resolve(tenant string, id RuleID, now time.Time) Stage {
	if m, ok := s.perTenant[tenant]; ok {
		if o, ok := m[id]; ok && !expired(o, now) {
			return o.Stage
		}
	}
	if o, ok := s.global[id]; ok && !expired(o, now) {
		return o.Stage
	}
	return s.base.StageFor(id)
}

func expired(o StageOverride, now time.Time) bool {
	return !o.ExpiresAt.IsZero() && !now.Before(o.ExpiresAt)
}

// Bind freezes the resolver for one tenant at one instant, producing the flat lookup a RuleSet
// consumes. Binding once per request rather than per rule keeps evaluation free of map lookups
// that depend on a clock.
func (s *Stages) Bind(tenant string, now time.Time) StageLookup {
	return StageFunc(func(id RuleID) Stage { return s.Resolve(tenant, id, now) })
}

// ActiveOverrides lists the unexpired overrides for tenant at now, newest rule order, for the
// operator view that answers "what is this tenant not subject to".
func (s *Stages) ActiveOverrides(tenant string, now time.Time) []RuleID {
	seen := map[RuleID]struct{}{}
	for id, o := range s.global {
		if !expired(o, now) {
			seen[id] = struct{}{}
		}
	}
	for id, o := range s.perTenant[tenant] {
		if !expired(o, now) {
			seen[id] = struct{}{}
		}
	}
	out := make([]RuleID, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MetricHook receives every evaluated outcome, including the shadow outcomes the report drops.
//
// This is the whole point of shadow mode: the outcome has to reach a counter even when it
// reaches nothing else, because the promotion decision is made from
// pp_validation_outcomes_total{rule,stage,result} and from nothing else. The hook is an
// interface on the set rather than a package-level metric so that the engine stays free of an
// OpenTelemetry dependency and so that tests can assert recording happened.
type MetricHook interface {
	// RecordOutcome is called once per evaluated rule, after stage demotion has been applied.
	// It must not block and must not panic; the engine calls it inside the evaluation loop.
	RecordOutcome(ctx context.Context, set string, o Outcome)
}

// MetricFunc adapts a function to MetricHook.
type MetricFunc func(ctx context.Context, set string, o Outcome)

// RecordOutcome satisfies MetricHook.
func (f MetricFunc) RecordOutcome(ctx context.Context, set string, o Outcome) { f(ctx, set, o) }
